/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreedto in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package txthrottler

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"google.golang.org/protobuf/encoding/prototext"

	"context"

	"vitess.io/vitess/go/vt/discovery"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/throttler"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/tabletenv"

	querypb "vitess.io/vitess/go/vt/proto/query"
	throttlerdatapb "vitess.io/vitess/go/vt/proto/throttlerdata"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
)

// TxThrottler throttles transactions based on replication lag.
// It's a thin wrapper around the throttler found in vitess/go/vt/throttler.
// It uses a discovery.HealthCheck to send replication-lag updates to the wrapped throttler.
//
// Intended Usage:
//   // Assuming topoServer is a topo.Server variable pointing to a Vitess topology server.
//   t := NewTxThrottler(config, topoServer)
//
//   // A transaction throttler must be opened before its first use:
//   if err := t.Open(keyspace, shard); err != nil {
//     return err
//   }
//
//   // Checking whether to throttle can be done as follows before starting a transaction.
//   if t.Throttle() {
//     return fmt.Errorf("Transaction throttled!")
//   } else {
//     // execute transaction.
//   }
//
//   // To release the resources used by the throttler the caller should call Close().
//   t.Close()
//
// A TxThrottler object is generally not thread-safe: at any given time at most one goroutine should
// be executing a method. The only exception is the 'Throttle' method where multiple goroutines are
// allowed to execute it concurrently.
type TxThrottler struct {
	// config stores the transaction throttler's configuration.
	// It is populated in NewTxThrottler and is not modified
	// since.
	config *txThrottlerConfig

	// state holds an open transaction throttler state. It is nil
	// if the TransactionThrottler is closed.
	state *txThrottlerState

	target *querypb.Target
}

// NewTxThrottler tries to construct a TxThrottler from the
// relevant fields in the tabletenv.Config object. It returns a disabled TxThrottler if
// any error occurs.
// This function calls tryCreateTxThrottler that does the actual creation work
// and returns an error if one occurred.
func NewTxThrottler(config *tabletenv.TabletConfig, topoServer *topo.Server) *TxThrottler {
	txThrottler, err := tryCreateTxThrottler(config, topoServer)
	if err != nil {
		log.Errorf("Error creating transaction throttler. Transaction throttling will"+
			" be disabled. Error: %v", err)
		txThrottler, err = newTxThrottler(&txThrottlerConfig{enabled: false})
		if err != nil {
			panic("BUG: Can't create a disabled transaction throttler")
		}
	} else {
		log.Infof("Initialized transaction throttler with config: %+v", txThrottler.config)
	}
	return txThrottler
}

// InitDBConfig initializes the target parameters for the throttler.
func (t *TxThrottler) InitDBConfig(target *querypb.Target) {
	t.target = proto.Clone(target).(*querypb.Target)
}

func tryCreateTxThrottler(config *tabletenv.TabletConfig, topoServer *topo.Server) (*TxThrottler, error) {
	if !config.EnableTxThrottler {
		return newTxThrottler(&txThrottlerConfig{enabled: false})
	}

	var throttlerConfig throttlerdatapb.Configuration
	if err := prototext.Unmarshal([]byte(config.TxThrottlerConfig), &throttlerConfig); err != nil {
		return nil, err
	}

	// Clone tsv.TxThrottlerHealthCheckCells so that we don't assume tsv.TxThrottlerHealthCheckCells
	// is immutable.
	healthCheckCells := make([]string, len(config.TxThrottlerHealthCheckCells))
	copy(healthCheckCells, config.TxThrottlerHealthCheckCells)

	return newTxThrottler(&txThrottlerConfig{
		enabled:          true,
		topoServer:       topoServer,
		throttlerConfig:  &throttlerConfig,
		healthCheckCells: healthCheckCells,
	})
}

// txThrottlerConfig holds the parameters that need to be
// passed when constructing a TxThrottler object.
type txThrottlerConfig struct {
	// enabled is true if the transaction throttler is enabled. All methods
	// of a disabled transaction throttler do nothing and Throttle() always
	// returns false.
	enabled bool

	topoServer      *topo.Server
	throttlerConfig *throttlerdatapb.Configuration
	// healthCheckCells stores the cell names in which running vttablets will be monitored for
	// replication lag.
	healthCheckCells []string
}

// ThrottlerInterface defines the public interface that is implemented by go/vt/throttler.Throttler
// It is only used here to allow mocking out a throttler object.
type ThrottlerInterface interface {
	Throttle(threadID int) time.Duration
	ThreadFinished(threadID int)
	Close()
	MaxRate() int64
	SetMaxRate(rate int64)
	RecordReplicationLag(time time.Time, th *discovery.TabletHealth)
	GetConfiguration() *throttlerdatapb.Configuration
	UpdateConfiguration(configuration *throttlerdatapb.Configuration, copyZeroValues bool) error
	ResetConfiguration()
}

// TopologyWatcherInterface defines the public interface that is implemented by
// discovery.LegacyTopologyWatcher. It is only used here to allow mocking out
// go/vt/discovery.LegacyTopologyWatcher.
type TopologyWatcherInterface interface {
	Start()
	Stop()
}

// txThrottlerState holds the state of an open TxThrottler object.
type txThrottlerState struct {
	// throttleMu serializes calls to throttler.Throttler.Throttle(threadId).
	// That method is required to be called in serial for each threadId.
	throttleMu      sync.Mutex
	throttler       ThrottlerInterface
	stopHealthCheck context.CancelFunc

	healthCheck      discovery.HealthCheck
	topologyWatchers []TopologyWatcherInterface
}

// These vars store the functions used to create the topo server, healthcheck,
// topology watchers and go/vt/throttler. These are provided here so that they can be overridden
// in tests to generate mocks.
type healthCheckFactoryFunc func(topoServer *topo.Server, cell string, cellsToWatch []string) discovery.HealthCheck
type topologyWatcherFactoryFunc func(topoServer *topo.Server, hc discovery.HealthCheck, cell, keyspace, shard string, refreshInterval time.Duration, topoReadConcurrency int) TopologyWatcherInterface
type throttlerFactoryFunc func(name, unit string, threadCount int, maxRate, maxReplicationLag int64) (ThrottlerInterface, error)

var (
	healthCheckFactory     healthCheckFactoryFunc
	topologyWatcherFactory topologyWatcherFactoryFunc
	throttlerFactory       throttlerFactoryFunc
)

func init() {
	resetTxThrottlerFactories()
}

func resetTxThrottlerFactories() {
	healthCheckFactory = func(topoServer *topo.Server, cell string, cellsToWatch []string) discovery.HealthCheck {
		return discovery.NewHealthCheck(context.Background(), discovery.DefaultHealthCheckRetryDelay, discovery.DefaultHealthCheckTimeout, topoServer, cell, strings.Join(cellsToWatch, ","))
	}
	topologyWatcherFactory = func(topoServer *topo.Server, hc discovery.HealthCheck, cell, keyspace, shard string, refreshInterval time.Duration, topoReadConcurrency int) TopologyWatcherInterface {
		return discovery.NewCellTabletsWatcher(context.Background(), topoServer, hc, discovery.NewFilterByKeyspace([]string{keyspace}), cell, refreshInterval, true, topoReadConcurrency)
	}
	throttlerFactory = func(name, unit string, threadCount int, maxRate, maxReplicationLag int64) (ThrottlerInterface, error) {
		return throttler.NewThrottler(name, unit, threadCount, maxRate, maxReplicationLag)
	}
}

// TxThrottlerName is the name the wrapped go/vt/throttler object will be registered with
// go/vt/throttler.GlobalManager.
const TxThrottlerName = "TransactionThrottler"

func newTxThrottler(config *txThrottlerConfig) (*TxThrottler, error) {
	if config.enabled {
		// Verify config.
		err := throttler.MaxReplicationLagModuleConfig{Configuration: config.throttlerConfig}.Verify()
		if err != nil {
			return nil, err
		}
		if len(config.healthCheckCells) == 0 {
			return nil, fmt.Errorf("empty healthCheckCells given. %+v", config)
		}
	}
	return &TxThrottler{
		config: config,
	}, nil
}

// Open opens the transaction throttler. It must be called prior to 'Throttle'.
func (t *TxThrottler) Open() error {
	if !t.config.enabled {
		return nil
	}
	if t.state != nil {
		return nil
	}
	log.Info("TxThrottler: opening")
	var err error
	t.state, err = newTxThrottlerState(t.config, t.target.Keyspace, t.target.Shard, t.target.Cell)
	return err
}

// Close closes the TxThrottler object and releases resources.
// It should be called after the throttler is no longer needed.
// It's ok to call this method on a closed throttler--in which case the method does nothing.
func (t *TxThrottler) Close() {
	if !t.config.enabled {
		return
	}
	if t.state == nil {
		return
	}
	t.state.deallocateResources()
	t.state = nil
	log.Info("TxThrottler: closed")
}

// Throttle should be called before a new transaction is started.
// It returns true if the transaction should not proceed (the caller
// should back off). Throttle requires that Open() was previously called
// successfully.
func (t *TxThrottler) Throttle() (result bool) {
	if !t.config.enabled {
		return false
	}
	if t.state == nil {
		panic("BUG: Throttle() called on a closed TxThrottler")
	}
	return t.state.throttle()
}

func newTxThrottlerState(config *txThrottlerConfig, keyspace, shard, cell string) (*txThrottlerState, error) {
	t, err := throttlerFactory(
		TxThrottlerName,
		"TPS",                           /* unit */
		1,                               /* threadCount */
		throttler.MaxRateModuleDisabled, /* maxRate */
		config.throttlerConfig.MaxReplicationLagSec /* maxReplicationLag */)
	if err != nil {
		return nil, err
	}
	if err := t.UpdateConfiguration(config.throttlerConfig, true /* copyZeroValues */); err != nil {
		t.Close()
		return nil, err
	}
	result := &txThrottlerState{
		throttler: t,
	}
	createTxThrottlerHealthCheck(config, result, cell)

	result.topologyWatchers = make(
		[]TopologyWatcherInterface, 0, len(config.healthCheckCells))
	for _, cell := range config.healthCheckCells {
		result.topologyWatchers = append(
			result.topologyWatchers,
			topologyWatcherFactory(
				config.topoServer,
				result.healthCheck,
				cell,
				keyspace,
				shard,
				discovery.DefaultTopologyWatcherRefreshInterval,
				discovery.DefaultTopoReadConcurrency))
	}
	return result, nil
}

func createTxThrottlerHealthCheck(config *txThrottlerConfig, result *txThrottlerState, cell string) {
	ctx, cancel := context.WithCancel(context.Background())
	result.stopHealthCheck = cancel
	result.healthCheck = healthCheckFactory(config.topoServer, cell, config.healthCheckCells)
	ch := result.healthCheck.Subscribe()
	go func(ctx context.Context) {
		for {
			select {
			case <-ctx.Done():
				return
			case th := <-ch:
				result.StatsUpdate(th)
			}
		}
	}(ctx)
}

func (ts *txThrottlerState) throttle() bool {
	if ts.throttler == nil {
		panic("BUG: throttle called after deallocateResources was called.")
	}
	// Serialize calls to ts.throttle.Throttle()
	ts.throttleMu.Lock()
	defer ts.throttleMu.Unlock()
	return ts.throttler.Throttle(0 /* threadId */) > 0
}

func (ts *txThrottlerState) deallocateResources() {
	// We don't really need to nil out the fields here
	// as deallocateResources is not expected to be called
	// more than once, but it doesn't hurt to do so.
	for _, watcher := range ts.topologyWatchers {
		watcher.Stop()
	}
	ts.topologyWatchers = nil

	ts.healthCheck.Close()
	ts.healthCheck = nil

	// After ts.healthCheck is closed txThrottlerState.StatsUpdate() is guaranteed not
	// to be executing, so we can safely close the throttler.
	ts.throttler.Close()
	ts.throttler = nil
}

// StatsUpdate updates the health of a tablet with the given healthcheck.
func (ts *txThrottlerState) StatsUpdate(tabletStats *discovery.TabletHealth) {
	// Ignore PRIMARY and RDONLY stats.
	// We currently do not monitor RDONLY tablets for replication lag. RDONLY tablets are not
	// candidates for becoming primary during failover, and it's acceptable to serve somewhat
	// stale date from these.
	// TODO(erez): If this becomes necessary, we can add a configuration option that would
	// determine whether we consider RDONLY tablets here, as well.
	if tabletStats.Target.TabletType != topodatapb.TabletType_REPLICA {
		return
	}
	ts.throttler.RecordReplicationLag(time.Now(), tabletStats)
}
