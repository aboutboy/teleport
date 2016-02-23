var React = require('react');
var render = require('react-dom').render;
var { Router, Route, Redirect, IndexRoute, browserHistory } = require('react-router');
var { App, Login, Nodes, Sessions, NewUser } = require('./components');
var auth = require('./auth');
var session = require('./session');
var cfg = require('./config');

// init session
session.init();

function requireAuth(nextState, replace, cb) {
  auth.ensureUser()
    .done(()=> cb())
    .fail(()=>{
      replace({redirectTo: nextState.location.pathname }, cfg.routes.login);
      cb();
    });
}

function handleLogout(nextState, replace, cb){
  auth.logout();
  // going back will hit requireAuth handler which will redirect it to the login page
  session.getHistory().goBack();
}

render((
  <Router history={session.getHistory()}>
    <Route path={cfg.routes.login} component={Login}/>
    <Route path={cfg.routes.logout} onEnter={handleLogout}/>
    <Route path={cfg.routes.newUser} component={NewUser}/>
    <Route path={cfg.routes.app} component={App}>
      <Route path={cfg.routes.nodes} component={Nodes}/>
      <Route path={cfg.routes.sessions} component={Sessions}/>
    </Route>
  </Router>
), document.getElementById("app"));
