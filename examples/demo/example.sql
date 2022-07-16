insert into user (id, email, state) values (1, 'alice@gmail.com', 'A');
insert into user (id, email, state) values (2, 'bob@hotmail.com', 'A');
insert into team (id, name, state) values (11, 'Acme', 'A'), (22, 'Beta', 'A');
insert into team_member (team, user) values (11, 1), (11, 2), (22, 1);

select user.id, team.id
from user
         join team_member on user.id = team_member.user
         join team on team_member.team = team.id
where user.email = 'alice@gmail.com'
  and user.state = 'A'
  and team.state = 'A';