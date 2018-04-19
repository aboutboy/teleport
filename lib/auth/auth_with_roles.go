/*
Copyright 2015 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package auth

import (
	"context"
	"io"
	"net/url"
	"time"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/session"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/gravitational/trace"
	"github.com/tstranex/u2f"
)

type AuthWithRoles struct {
	authServer *AuthServer
	checker    services.AccessChecker
	user       services.User
	sessions   session.Service
	alog       events.IAuditLog
}

func (a *AuthWithRoles) actionWithContext(ctx *services.Context, namespace string, resource string, action string) error {
	return a.checker.CheckAccessToRule(ctx, namespace, resource, action)
}

func (a *AuthWithRoles) action(namespace string, resource string, action string) error {
	return a.checker.CheckAccessToRule(&services.Context{User: a.user}, namespace, resource, action)
}

// currentUserAction is a special checker that allows certain actions for users
// even if they are not admins, e.g. update their own passwords,
// or generate certificates, otherwise it will require admin privileges
func (a *AuthWithRoles) currentUserAction(username string) error {
	if username == a.user.GetName() {
		return nil
	}
	return a.checker.CheckAccessToRule(&services.Context{User: a.user},
		defaults.Namespace, services.KindUser, services.VerbCreate)
}

// authConnectorAction is a special checker that grants access to auth
// connectors. It first checks if you have access to the specific connector.
// If not, it checks if the requester has the meta KindAuthConnector access
// (which grants access to all connectors).
func (a *AuthWithRoles) authConnectorAction(namespace string, resource string, verb string) error {
	if err := a.checker.CheckAccessToRule(&services.Context{User: a.user}, namespace, resource, verb); err != nil {
		if err := a.checker.CheckAccessToRule(&services.Context{User: a.user}, namespace, services.KindAuthConnector, verb); err != nil {
			return trace.Wrap(err)
		}
	}
	return nil
}

// AuthenticateWebUser authenticates web user, creates and  returns web session
// in case if authentication is successfull
func (a *AuthWithRoles) AuthenticateWebUser(req AuthenticateUserRequest) (services.WebSession, error) {
	// authentication request has it's own authentication, however this limits the requests
	// types to proxies to make it harder to break
	if !a.checker.HasRole(string(teleport.RoleProxy)) {
		return nil, trace.AccessDenied("this request can be only executed by proxy")
	}
	return a.authServer.AuthenticateWebUser(req)
}

// AuthenticateSSHUser authenticates SSH console user, creates and  returns a pair of signed TLS and SSH
// short lived certificates as a result
func (a *AuthWithRoles) AuthenticateSSHUser(req AuthenticateSSHRequest) (*SSHLoginResponse, error) {
	// authentication request has it's own authentication, however this limits the requests
	// types to proxies to make it harder to break
	if !a.checker.HasRole(string(teleport.RoleProxy)) {
		return nil, trace.AccessDenied("this request can be only executed by proxy")
	}
	return a.authServer.AuthenticateSSHUser(req)
}

func (a *AuthWithRoles) GetSessions(namespace string) ([]session.Session, error) {
	if err := a.action(namespace, services.KindSSHSession, services.VerbList); err != nil {
		return nil, trace.Wrap(err)
	}

	return a.sessions.GetSessions(namespace)
}

func (a *AuthWithRoles) GetSession(namespace string, id session.ID) (*session.Session, error) {
	if err := a.action(namespace, services.KindSSHSession, services.VerbRead); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.sessions.GetSession(namespace, id)
}

func (a *AuthWithRoles) CreateSession(s session.Session) error {
	if err := a.action(s.Namespace, services.KindSSHSession, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	return a.sessions.CreateSession(s)
}

func (a *AuthWithRoles) UpdateSession(req session.UpdateRequest) error {
	if err := a.action(req.Namespace, services.KindSSHSession, services.VerbUpdate); err != nil {
		return trace.Wrap(err)
	}
	return a.sessions.UpdateSession(req)
}

func (a *AuthWithRoles) CreateCertAuthority(ca services.CertAuthority) error {
	return trace.BadParameter("not implemented")
}

// Rotate starts or restarts certificate rotation process
func (a *AuthWithRoles) RotateCertAuthority(req RotateRequest) error {
	if err := req.CheckAndSetDefaults(); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindCertAuthority, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindCertAuthority, services.VerbUpdate); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.RotateCertAuthority(req)
}

func (a *AuthWithRoles) UpsertCertAuthority(ca services.CertAuthority) error {
	ctx := &services.Context{User: a.user, Resource: ca}
	if err := a.actionWithContext(ctx, defaults.Namespace, services.KindCertAuthority, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	if err := a.actionWithContext(ctx, defaults.Namespace, services.KindCertAuthority, services.VerbUpdate); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.UpsertCertAuthority(ca)
}

func (a *AuthWithRoles) CompareAndSwapCertAuthority(new, existing services.CertAuthority) error {
	if err := a.action(defaults.Namespace, services.KindCertAuthority, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindCertAuthority, services.VerbUpdate); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.CompareAndSwapCertAuthority(new, existing)
}

func (a *AuthWithRoles) GetCertAuthorities(caType services.CertAuthType, loadKeys bool) ([]services.CertAuthority, error) {
	if err := a.action(defaults.Namespace, services.KindCertAuthority, services.VerbList); err != nil {
		return nil, trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindCertAuthority, services.VerbReadNoSecrets); err != nil {
		return nil, trace.Wrap(err)
	}
	if loadKeys {
		if err := a.action(defaults.Namespace, services.KindCertAuthority, services.VerbRead); err != nil {
			return nil, trace.Wrap(err)
		}
	}
	return a.authServer.GetCertAuthorities(caType, loadKeys)
}

func (a *AuthWithRoles) GetCertAuthority(id services.CertAuthID, loadKeys bool) (services.CertAuthority, error) {
	if err := a.action(defaults.Namespace, services.KindCertAuthority, services.VerbReadNoSecrets); err != nil {
		return nil, trace.Wrap(err)
	}
	if loadKeys {
		if err := a.action(defaults.Namespace, services.KindCertAuthority, services.VerbRead); err != nil {
			return nil, trace.Wrap(err)
		}
	}
	return a.authServer.GetCertAuthority(id, loadKeys)
}

func (a *AuthWithRoles) GetDomainName() (string, error) {
	// anyone can read it, no harm in that
	return a.authServer.GetDomainName()
}

func (a *AuthWithRoles) GetLocalClusterName() (string, error) {
	// anyone can read it, no harm in that
	return a.authServer.GetLocalClusterName()
}

func (a *AuthWithRoles) UpsertLocalClusterName(clusterName string) error {
	if err := a.action(defaults.Namespace, services.KindAuthServer, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindAuthServer, services.VerbUpdate); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.UpsertLocalClusterName(clusterName)
}

func (a *AuthWithRoles) DeleteCertAuthority(id services.CertAuthID) error {
	if err := a.action(defaults.Namespace, services.KindCertAuthority, services.VerbDelete); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.DeleteCertAuthority(id)
}

func (a *AuthWithRoles) ActivateCertAuthority(id services.CertAuthID) error {
	return trace.BadParameter("not implemented")
}

func (a *AuthWithRoles) DeactivateCertAuthority(id services.CertAuthID) error {
	return trace.BadParameter("not implemented")
}

func (a *AuthWithRoles) GenerateToken(req GenerateTokenRequest) (string, error) {
	if err := a.action(defaults.Namespace, services.KindToken, services.VerbCreate); err != nil {
		return "", trace.Wrap(err)
	}
	return a.authServer.GenerateToken(req)
}

func (a *AuthWithRoles) RegisterUsingToken(req RegisterUsingTokenRequest) (*PackedKeys, error) {
	// tokens have authz mechanism  on their own, no need to check
	return a.authServer.RegisterUsingToken(req)
}

func (a *AuthWithRoles) RegisterNewAuthServer(token string) error {
	// tokens have authz mechanism  on their own, no need to check
	return a.authServer.RegisterNewAuthServer(token)
}

// GenerateServerKeys generates new host private keys and certificates (signed
// by the host certificate authority) for a node.
func (a *AuthWithRoles) GenerateServerKeys(req GenerateServerKeysRequest) (*PackedKeys, error) {
	clusterName, err := a.authServer.GetDomainName()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	// username is hostID + cluster name, so make sure server requests new keys for itself
	if a.user.GetName() != HostFQDN(req.HostID, clusterName) {
		return nil, trace.AccessDenied("username mismatch %q and %q", a.user.GetName(), HostFQDN(req.HostID, clusterName))
	}
	existingRoles, err := teleport.NewRoles(a.user.GetRoles())
	if err != nil {
		return nil, trace.Wrap(err)
	}
	// prohibit privilege escalations through role changes
	if !existingRoles.Equals(req.Roles) {
		return nil, trace.AccessDenied("roles do not match: %v and %v", existingRoles, req.Roles)
	}
	return a.authServer.GenerateServerKeys(req)
}

func (a *AuthWithRoles) UpsertNode(s services.Server) error {
	if err := a.action(s.GetNamespace(), services.KindNode, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(s.GetNamespace(), services.KindNode, services.VerbUpdate); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.UpsertNode(s)
}

func (a *AuthWithRoles) GetNodes(namespace string) ([]services.Server, error) {
	if err := a.action(namespace, services.KindNode, services.VerbList); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.GetNodes(namespace)
}

func (a *AuthWithRoles) UpsertAuthServer(s services.Server) error {
	if err := a.action(defaults.Namespace, services.KindAuthServer, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindAuthServer, services.VerbUpdate); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.UpsertAuthServer(s)
}

func (a *AuthWithRoles) GetAuthServers() ([]services.Server, error) {
	if err := a.action(defaults.Namespace, services.KindAuthServer, services.VerbList); err != nil {
		return nil, trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindAuthServer, services.VerbRead); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.GetAuthServers()
}

func (a *AuthWithRoles) UpsertProxy(s services.Server) error {
	if err := a.action(defaults.Namespace, services.KindProxy, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindProxy, services.VerbUpdate); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.UpsertProxy(s)
}

func (a *AuthWithRoles) GetProxies() ([]services.Server, error) {
	if err := a.action(defaults.Namespace, services.KindProxy, services.VerbList); err != nil {
		return nil, trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindProxy, services.VerbRead); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.GetProxies()
}

func (a *AuthWithRoles) UpsertReverseTunnel(r services.ReverseTunnel) error {
	if err := a.action(defaults.Namespace, services.KindReverseTunnel, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindReverseTunnel, services.VerbUpdate); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.UpsertReverseTunnel(r)
}

func (a *AuthWithRoles) GetReverseTunnel(name string) (services.ReverseTunnel, error) {
	if err := a.action(defaults.Namespace, services.KindReverseTunnel, services.VerbRead); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.GetReverseTunnel(name)
}

func (a *AuthWithRoles) GetReverseTunnels() ([]services.ReverseTunnel, error) {
	if err := a.action(defaults.Namespace, services.KindReverseTunnel, services.VerbList); err != nil {
		return nil, trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindReverseTunnel, services.VerbRead); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.GetReverseTunnels()
}

func (a *AuthWithRoles) DeleteReverseTunnel(domainName string) error {
	if err := a.action(defaults.Namespace, services.KindReverseTunnel, services.VerbDelete); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.DeleteReverseTunnel(domainName)
}

func (a *AuthWithRoles) DeleteToken(token string) error {
	if err := a.action(defaults.Namespace, services.KindToken, services.VerbDelete); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.DeleteToken(token)
}

func (a *AuthWithRoles) GetTokens() ([]services.ProvisionToken, error) {
	if err := a.action(defaults.Namespace, services.KindToken, services.VerbList); err != nil {
		return nil, trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindToken, services.VerbRead); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.GetTokens()
}

func (a *AuthWithRoles) GetToken(token string) (*services.ProvisionToken, error) {
	if err := a.action(defaults.Namespace, services.KindToken, services.VerbRead); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.GetToken(token)
}

func (a *AuthWithRoles) UpsertToken(token string, roles teleport.Roles, ttl time.Duration) error {
	if err := a.action(defaults.Namespace, services.KindToken, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindToken, services.VerbUpdate); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.UpsertToken(token, roles, ttl)
}

func (a *AuthWithRoles) UpsertPassword(user string, password []byte) error {
	if err := a.currentUserAction(user); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.UpsertPassword(user, password)
}

func (a *AuthWithRoles) ChangePassword(req services.ChangePasswordReq) error {
	if err := a.currentUserAction(req.User); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.ChangePassword(req)
}

func (a *AuthWithRoles) CheckPassword(user string, password []byte, otpToken string) error {
	if err := a.currentUserAction(user); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.CheckPassword(user, password, otpToken)
}

func (a *AuthWithRoles) UpsertTOTP(user string, otpSecret string) error {
	if err := a.currentUserAction(user); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.UpsertTOTP(user, otpSecret)
}

func (a *AuthWithRoles) GetOTPData(user string) (string, []byte, error) {
	if err := a.currentUserAction(user); err != nil {
		return "", nil, trace.Wrap(err)
	}
	return a.authServer.GetOTPData(user)
}

// DELETE IN: 2.6.0
// This method is no longer used in 2.5.0 and is replaced by AuthenticateUser methods
func (a *AuthWithRoles) SignIn(user string, password []byte) (services.WebSession, error) {
	if err := a.currentUserAction(user); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.SignIn(user, password)
}

func (a *AuthWithRoles) PreAuthenticatedSignIn(user string) (services.WebSession, error) {
	if err := a.currentUserAction(user); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.PreAuthenticatedSignIn(user)
}

func (a *AuthWithRoles) GetU2FSignRequest(user string, password []byte) (*u2f.SignRequest, error) {
	// we are already checking password here, no need to extra permission check
	// anyone who has user's password can generate sign request
	return a.authServer.U2FSignRequest(user, password)
}

func (a *AuthWithRoles) CreateWebSession(user string) (services.WebSession, error) {
	if err := a.currentUserAction(user); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.CreateWebSession(user)
}

func (a *AuthWithRoles) ExtendWebSession(user, prevSessionID string) (services.WebSession, error) {
	if err := a.currentUserAction(user); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.ExtendWebSession(user, prevSessionID)
}

func (a *AuthWithRoles) GetWebSessionInfo(user string, sid string) (services.WebSession, error) {
	if err := a.currentUserAction(user); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.GetWebSessionInfo(user, sid)
}

func (a *AuthWithRoles) DeleteWebSession(user string, sid string) error {
	if err := a.currentUserAction(user); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.DeleteWebSession(user, sid)
}

func (a *AuthWithRoles) GetUsers() ([]services.User, error) {
	if err := a.action(defaults.Namespace, services.KindUser, services.VerbList); err != nil {
		return nil, trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindUser, services.VerbRead); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.GetUsers()
}

func (a *AuthWithRoles) GetUser(name string) (services.User, error) {
	if err := a.currentUserAction(name); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.Identity.GetUser(name)
}

func (a *AuthWithRoles) DeleteUser(user string) error {
	if err := a.action(defaults.Namespace, services.KindUser, services.VerbDelete); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.DeleteUser(user)
}

func (a *AuthWithRoles) GenerateKeyPair(pass string) ([]byte, []byte, error) {
	if err := a.action(defaults.Namespace, services.KindKeyPair, services.VerbCreate); err != nil {
		return nil, nil, trace.Wrap(err)
	}
	return a.authServer.GenerateKeyPair(pass)
}

func (a *AuthWithRoles) GenerateHostCert(
	key []byte, hostID, nodeName string, principals []string, clusterName string, roles teleport.Roles, ttl time.Duration) ([]byte, error) {

	if err := a.action(defaults.Namespace, services.KindHostCert, services.VerbCreate); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.GenerateHostCert(key, hostID, nodeName, principals, clusterName, roles, ttl)
}

func (a *AuthWithRoles) GenerateUserCert(key []byte, username string, ttl time.Duration, compatibility string) ([]byte, error) {
	if err := a.currentUserAction(username); err != nil {
		return nil, trace.AccessDenied("%v cannot request a certificate for %v", a.user.GetName(), username)
	}
	// notice that user requesting the certificate and the user currently
	// authenticated may differ (e.g. admin generates certificate for the user scenario)
	// so we fetch user's permissions
	checker := a.checker
	var user services.User
	var err error
	if a.user.GetName() != username {
		user, err = a.GetUser(username)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		checker, err = services.FetchRoles(user.GetRoles(), a.authServer, user.GetTraits())
		if err != nil {
			return nil, trace.Wrap(err)
		}
	} else {
		user = a.user
	}
	certs, err := a.authServer.generateUserCert(certRequest{
		user:          user,
		roles:         checker,
		ttl:           ttl,
		compatibility: compatibility,
		publicKey:     key,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return certs.ssh, nil
}

func (a *AuthWithRoles) CreateSignupToken(user services.UserV1, ttl time.Duration) (token string, e error) {
	if err := a.action(defaults.Namespace, services.KindUser, services.VerbCreate); err != nil {
		return "", trace.Wrap(err)
	}
	return a.authServer.CreateSignupToken(user, ttl)
}

func (a *AuthWithRoles) GetSignupTokenData(token string) (user string, otpQRCode []byte, err error) {
	// signup token are their own authz resource
	return a.authServer.GetSignupTokenData(token)
}

func (a *AuthWithRoles) GetSignupToken(token string) (*services.SignupToken, error) {
	// signup token are their own authz resource
	return a.authServer.GetSignupToken(token)
}

func (a *AuthWithRoles) GetSignupU2FRegisterRequest(token string) (u2fRegisterRequest *u2f.RegisterRequest, e error) {
	// signup token are their own authz resource
	return a.authServer.CreateSignupU2FRegisterRequest(token)
}

func (a *AuthWithRoles) CreateUserWithOTP(token, password, otpToken string) (services.WebSession, error) {
	// tokens are their own authz mechanism, no need to double check
	return a.authServer.CreateUserWithOTP(token, password, otpToken)
}

func (a *AuthWithRoles) CreateUserWithoutOTP(token string, password string) (services.WebSession, error) {
	// tokens are their own authz mechanism, no need to double check
	return a.authServer.CreateUserWithoutOTP(token, password)
}

func (a *AuthWithRoles) CreateUserWithU2FToken(token string, password string, u2fRegisterResponse u2f.RegisterResponse) (services.WebSession, error) {
	// signup tokens are their own authz resource
	return a.authServer.CreateUserWithU2FToken(token, password, u2fRegisterResponse)
}

func (a *AuthWithRoles) UpsertUser(u services.User) error {
	if err := a.action(defaults.Namespace, services.KindUser, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindUser, services.VerbUpdate); err != nil {
		return trace.Wrap(err)
	}

	createdBy := u.GetCreatedBy()
	if createdBy.IsEmpty() {
		u.SetCreatedBy(services.CreatedBy{
			User: services.UserRef{Name: a.user.GetName()},
		})
	}
	return a.authServer.UpsertUser(u)
}

func (a *AuthWithRoles) UpsertOIDCConnector(connector services.OIDCConnector) error {
	if err := a.authConnectorAction(defaults.Namespace, services.KindOIDC, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	if err := a.authConnectorAction(defaults.Namespace, services.KindOIDC, services.VerbUpdate); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.UpsertOIDCConnector(connector)
}

func (a *AuthWithRoles) GetOIDCConnector(id string, withSecrets bool) (services.OIDCConnector, error) {
	if err := a.authConnectorAction(defaults.Namespace, services.KindOIDC, services.VerbReadNoSecrets); err != nil {
		return nil, trace.Wrap(err)
	}
	if withSecrets {
		if err := a.authConnectorAction(defaults.Namespace, services.KindOIDC, services.VerbRead); err != nil {
			return nil, trace.Wrap(err)
		}
	}
	return a.authServer.Identity.GetOIDCConnector(id, withSecrets)
}

func (a *AuthWithRoles) GetOIDCConnectors(withSecrets bool) ([]services.OIDCConnector, error) {
	if err := a.authConnectorAction(defaults.Namespace, services.KindOIDC, services.VerbList); err != nil {
		return nil, trace.Wrap(err)
	}
	if err := a.authConnectorAction(defaults.Namespace, services.KindOIDC, services.VerbReadNoSecrets); err != nil {
		return nil, trace.Wrap(err)
	}
	if withSecrets {
		if err := a.authConnectorAction(defaults.Namespace, services.KindOIDC, services.VerbRead); err != nil {
			return nil, trace.Wrap(err)
		}
	}
	return a.authServer.Identity.GetOIDCConnectors(withSecrets)
}

func (a *AuthWithRoles) CreateOIDCAuthRequest(req services.OIDCAuthRequest) (*services.OIDCAuthRequest, error) {
	if err := a.action(defaults.Namespace, services.KindOIDCRequest, services.VerbCreate); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.CreateOIDCAuthRequest(req)
}

func (a *AuthWithRoles) ValidateOIDCAuthCallback(q url.Values) (*OIDCAuthResponse, error) {
	// auth callback is it's own authz, no need to check extra permissions
	return a.authServer.ValidateOIDCAuthCallback(q)
}

func (a *AuthWithRoles) DeleteOIDCConnector(connectorID string) error {
	if err := a.authConnectorAction(defaults.Namespace, services.KindOIDC, services.VerbDelete); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.DeleteOIDCConnector(connectorID)
}

func (a *AuthWithRoles) CreateSAMLConnector(connector services.SAMLConnector) error {
	if err := a.authConnectorAction(defaults.Namespace, services.KindSAML, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.UpsertSAMLConnector(connector)
}

func (a *AuthWithRoles) UpsertSAMLConnector(connector services.SAMLConnector) error {
	if err := a.authConnectorAction(defaults.Namespace, services.KindSAML, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	if err := a.authConnectorAction(defaults.Namespace, services.KindSAML, services.VerbUpdate); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.UpsertSAMLConnector(connector)
}

func (a *AuthWithRoles) GetSAMLConnector(id string, withSecrets bool) (services.SAMLConnector, error) {
	if err := a.authConnectorAction(defaults.Namespace, services.KindSAML, services.VerbReadNoSecrets); err != nil {
		return nil, trace.Wrap(err)
	}
	if withSecrets {
		if err := a.authConnectorAction(defaults.Namespace, services.KindSAML, services.VerbRead); err != nil {
			return nil, trace.Wrap(err)
		}
	}
	return a.authServer.Identity.GetSAMLConnector(id, withSecrets)
}

func (a *AuthWithRoles) GetSAMLConnectors(withSecrets bool) ([]services.SAMLConnector, error) {
	if err := a.authConnectorAction(defaults.Namespace, services.KindSAML, services.VerbList); err != nil {
		return nil, trace.Wrap(err)
	}
	if err := a.authConnectorAction(defaults.Namespace, services.KindSAML, services.VerbReadNoSecrets); err != nil {
		return nil, trace.Wrap(err)
	}
	if withSecrets {
		if err := a.authConnectorAction(defaults.Namespace, services.KindSAML, services.VerbRead); err != nil {
			return nil, trace.Wrap(err)
		}
	}
	return a.authServer.Identity.GetSAMLConnectors(withSecrets)
}

func (a *AuthWithRoles) CreateSAMLAuthRequest(req services.SAMLAuthRequest) (*services.SAMLAuthRequest, error) {
	if err := a.action(defaults.Namespace, services.KindSAMLRequest, services.VerbCreate); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.CreateSAMLAuthRequest(req)
}

func (a *AuthWithRoles) ValidateSAMLResponse(re string) (*SAMLAuthResponse, error) {
	// auth callback is it's own authz, no need to check extra permissions
	return a.authServer.ValidateSAMLResponse(re)
}

func (a *AuthWithRoles) DeleteSAMLConnector(connectorID string) error {
	if err := a.authConnectorAction(defaults.Namespace, services.KindSAML, services.VerbDelete); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.DeleteSAMLConnector(connectorID)
}

func (a *AuthWithRoles) CreateGithubConnector(connector services.GithubConnector) error {
	if err := a.authConnectorAction(defaults.Namespace, services.KindGithub, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.CreateGithubConnector(connector)
}

func (a *AuthWithRoles) UpsertGithubConnector(connector services.GithubConnector) error {
	if err := a.authConnectorAction(defaults.Namespace, services.KindGithub, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.UpsertGithubConnector(connector)
}

func (a *AuthWithRoles) GetGithubConnector(id string, withSecrets bool) (services.GithubConnector, error) {
	if err := a.authConnectorAction(defaults.Namespace, services.KindGithub, services.VerbReadNoSecrets); err != nil {
		return nil, trace.Wrap(err)
	}
	if withSecrets {
		if err := a.authConnectorAction(defaults.Namespace, services.KindGithub, services.VerbRead); err != nil {
			return nil, trace.Wrap(err)
		}
	}
	return a.authServer.Identity.GetGithubConnector(id, withSecrets)
}

func (a *AuthWithRoles) GetGithubConnectors(withSecrets bool) ([]services.GithubConnector, error) {
	if err := a.authConnectorAction(defaults.Namespace, services.KindGithub, services.VerbList); err != nil {
		return nil, trace.Wrap(err)
	}
	if err := a.authConnectorAction(defaults.Namespace, services.KindGithub, services.VerbReadNoSecrets); err != nil {
		return nil, trace.Wrap(err)
	}
	if withSecrets {
		if err := a.authConnectorAction(defaults.Namespace, services.KindGithub, services.VerbRead); err != nil {
			return nil, trace.Wrap(err)
		}
	}
	return a.authServer.Identity.GetGithubConnectors(withSecrets)
}

func (a *AuthWithRoles) DeleteGithubConnector(id string) error {
	if err := a.authConnectorAction(defaults.Namespace, services.KindGithub, services.VerbDelete); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.DeleteGithubConnector(id)
}

func (a *AuthWithRoles) CreateGithubAuthRequest(req services.GithubAuthRequest) (*services.GithubAuthRequest, error) {
	if err := a.action(defaults.Namespace, services.KindGithubRequest, services.VerbCreate); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.CreateGithubAuthRequest(req)
}

func (a *AuthWithRoles) ValidateGithubAuthCallback(q url.Values) (*GithubAuthResponse, error) {
	return a.authServer.ValidateGithubAuthCallback(q)
}

func (a *AuthWithRoles) EmitAuditEvent(eventType string, fields events.EventFields) error {
	if err := a.action(defaults.Namespace, services.KindEvent, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindEvent, services.VerbUpdate); err != nil {
		return trace.Wrap(err)
	}
	return a.alog.EmitAuditEvent(eventType, fields)
}

func (a *AuthWithRoles) PostSessionSlice(slice events.SessionSlice) error {
	if err := a.action(slice.Namespace, services.KindEvent, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(slice.Namespace, services.KindEvent, services.VerbUpdate); err != nil {
		return trace.Wrap(err)
	}
	return a.alog.PostSessionSlice(slice)
}

func (a *AuthWithRoles) PostSessionChunk(namespace string, sid session.ID, reader io.Reader) error {
	if err := a.action(namespace, services.KindEvent, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(namespace, services.KindEvent, services.VerbUpdate); err != nil {
		return trace.Wrap(err)
	}
	return a.alog.PostSessionChunk(namespace, sid, reader)
}

func (a *AuthWithRoles) UploadSessionRecording(r events.SessionRecording) error {
	if err := r.CheckAndSetDefaults(); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(r.Namespace, services.KindEvent, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(r.Namespace, services.KindEvent, services.VerbUpdate); err != nil {
		return trace.Wrap(err)
	}
	return a.alog.UploadSessionRecording(r)
}

func (a *AuthWithRoles) GetSessionChunk(namespace string, sid session.ID, offsetBytes, maxBytes int) ([]byte, error) {
	if err := a.action(namespace, services.KindSession, services.VerbRead); err != nil {
		return nil, trace.Wrap(err)
	}

	return a.alog.GetSessionChunk(namespace, sid, offsetBytes, maxBytes)
}

func (a *AuthWithRoles) GetSessionEvents(namespace string, sid session.ID, afterN int, includePrintEvents bool) ([]events.EventFields, error) {
	if err := a.action(namespace, services.KindSession, services.VerbRead); err != nil {
		return nil, trace.Wrap(err)
	}

	return a.alog.GetSessionEvents(namespace, sid, afterN, includePrintEvents)
}

func (a *AuthWithRoles) SearchEvents(from, to time.Time, query string, limit int) ([]events.EventFields, error) {
	if err := a.action(defaults.Namespace, services.KindEvent, services.VerbList); err != nil {
		return nil, trace.Wrap(err)
	}

	return a.alog.SearchEvents(from, to, query, limit)
}

func (a *AuthWithRoles) SearchSessionEvents(from, to time.Time, limit int) ([]events.EventFields, error) {
	if err := a.action(defaults.Namespace, services.KindSession, services.VerbList); err != nil {
		return nil, trace.Wrap(err)
	}

	return a.alog.SearchSessionEvents(from, to, limit)
}

// GetNamespaces returns a list of namespaces
func (a *AuthWithRoles) GetNamespaces() ([]services.Namespace, error) {
	if err := a.action(defaults.Namespace, services.KindNamespace, services.VerbList); err != nil {
		return nil, trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindNamespace, services.VerbRead); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.GetNamespaces()
}

// GetNamespace returns namespace by name
func (a *AuthWithRoles) GetNamespace(name string) (*services.Namespace, error) {
	if err := a.action(defaults.Namespace, services.KindNamespace, services.VerbRead); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.GetNamespace(name)
}

// UpsertNamespace upserts namespace
func (a *AuthWithRoles) UpsertNamespace(ns services.Namespace) error {
	if err := a.action(defaults.Namespace, services.KindNamespace, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindNamespace, services.VerbUpdate); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.UpsertNamespace(ns)
}

// DeleteNamespace deletes namespace by name
func (a *AuthWithRoles) DeleteNamespace(name string) error {
	if err := a.action(defaults.Namespace, services.KindNamespace, services.VerbDelete); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.DeleteNamespace(name)
}

// GetRoles returns a list of roles
func (a *AuthWithRoles) GetRoles() ([]services.Role, error) {
	if err := a.action(defaults.Namespace, services.KindRole, services.VerbList); err != nil {
		return nil, trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindRole, services.VerbRead); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.GetRoles()
}

// CreateRole creates a role.
func (a *AuthWithRoles) CreateRole(role services.Role, ttl time.Duration) error {
	return trace.BadParameter("not implemented")
}

// UpsertRole creates or updates role
func (a *AuthWithRoles) UpsertRole(role services.Role, ttl time.Duration) error {
	if err := a.action(defaults.Namespace, services.KindRole, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindRole, services.VerbUpdate); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.UpsertRole(role, ttl)
}

// GetRole returns role by name
func (a *AuthWithRoles) GetRole(name string) (services.Role, error) {
	if err := a.action(defaults.Namespace, services.KindRole, services.VerbRead); err != nil {
		// allow user to read roles assigned to them
		log.Infof("%v %v %v", a.user, a.user.GetRoles(), name)
		if !utils.SliceContainsStr(a.user.GetRoles(), name) {
			return nil, trace.Wrap(err)
		}
	}
	return a.authServer.GetRole(name)
}

// DeleteRole deletes role by name
func (a *AuthWithRoles) DeleteRole(name string) error {
	if err := a.action(defaults.Namespace, services.KindRole, services.VerbDelete); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.DeleteRole(name)
}

// GetClusterConfig gets cluster level configuration.
func (a *AuthWithRoles) GetClusterConfig() (services.ClusterConfig, error) {
	if err := a.action(defaults.Namespace, services.KindClusterConfig, services.VerbRead); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.GetClusterConfig()
}

// SetClusterConfig sets cluster level configuration.
func (a *AuthWithRoles) SetClusterConfig(c services.ClusterConfig) error {
	if err := a.action(defaults.Namespace, services.KindClusterConfig, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindClusterConfig, services.VerbUpdate); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.SetClusterConfig(c)
}

// GetClusterName gets the name of the cluster.
func (a *AuthWithRoles) GetClusterName() (services.ClusterName, error) {
	if err := a.action(defaults.Namespace, services.KindClusterName, services.VerbRead); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.GetClusterName()
}

// SetClusterName sets the name of the cluster. SetClusterName can only be called once.
func (a *AuthWithRoles) SetClusterName(c services.ClusterName) error {
	if err := a.action(defaults.Namespace, services.KindClusterName, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindClusterName, services.VerbUpdate); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.SetClusterName(c)
}

// GetStaticTokens gets the list of static tokens used to provision nodes.
func (a *AuthWithRoles) GetStaticTokens() (services.StaticTokens, error) {
	if err := a.action(defaults.Namespace, services.KindStaticTokens, services.VerbRead); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.GetStaticTokens()
}

// SetStaticTokens sets the list of static tokens used to provision nodes.
func (a *AuthWithRoles) SetStaticTokens(s services.StaticTokens) error {
	if err := a.action(defaults.Namespace, services.KindStaticTokens, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindStaticTokens, services.VerbUpdate); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.SetStaticTokens(s)
}

func (a *AuthWithRoles) GetAuthPreference() (services.AuthPreference, error) {
	if err := a.action(defaults.Namespace, services.KindClusterAuthPreference, services.VerbRead); err != nil {
		return nil, trace.Wrap(err)
	}

	return a.authServer.GetAuthPreference()
}

func (a *AuthWithRoles) SetAuthPreference(cap services.AuthPreference) error {
	if err := a.action(defaults.Namespace, services.KindClusterAuthPreference, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindClusterAuthPreference, services.VerbUpdate); err != nil {
		return trace.Wrap(err)
	}

	return a.authServer.SetAuthPreference(cap)
}

// DeleteAllCertAuthorities deletes all certificate authorities of a certain type
func (a *AuthWithRoles) DeleteAllCertAuthorities(caType services.CertAuthType) error {
	return trace.BadParameter("not implemented")
}

// DeleteAllCertNamespaces deletes all namespaces
func (a *AuthWithRoles) DeleteAllNamespaces() error {
	return trace.BadParameter("not implemented")
}

// DeleteAllReverseTunnels deletes all reverse tunnels
func (a *AuthWithRoles) DeleteAllReverseTunnels() error {
	return trace.BadParameter("not implemented")
}

// DeleteAllProxies deletes all proxies
func (a *AuthWithRoles) DeleteAllProxies() error {
	return trace.BadParameter("not implemented")
}

// DeleteAllNodes deletes all nodes in a given namespace
func (a *AuthWithRoles) DeleteAllNodes(namespace string) error {
	return trace.BadParameter("not implemented")
}

// DeleteAllRoles deletes all roles
func (a *AuthWithRoles) DeleteAllRoles() error {
	return trace.BadParameter("not implemented")
}

// DeleteAllUsers deletes all users
func (a *AuthWithRoles) DeleteAllUsers() error {
	return trace.BadParameter("not implemented")
}

func (a *AuthWithRoles) GetTrustedClusters() ([]services.TrustedCluster, error) {
	if err := a.action(defaults.Namespace, services.KindTrustedCluster, services.VerbList); err != nil {
		return nil, trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindTrustedCluster, services.VerbRead); err != nil {
		return nil, trace.Wrap(err)
	}

	return a.authServer.GetTrustedClusters()
}

func (a *AuthWithRoles) GetTrustedCluster(name string) (services.TrustedCluster, error) {
	if err := a.action(defaults.Namespace, services.KindTrustedCluster, services.VerbRead); err != nil {
		return nil, trace.Wrap(err)
	}

	return a.authServer.GetTrustedCluster(name)
}

func (a *AuthWithRoles) UpsertTrustedCluster(tc services.TrustedCluster) (services.TrustedCluster, error) {
	if err := a.action(defaults.Namespace, services.KindTrustedCluster, services.VerbCreate); err != nil {
		return nil, trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindTrustedCluster, services.VerbUpdate); err != nil {
		return nil, trace.Wrap(err)
	}

	return a.authServer.UpsertTrustedCluster(tc)
}

func (a *AuthWithRoles) ValidateTrustedCluster(validateRequest *ValidateTrustedClusterRequest) (*ValidateTrustedClusterResponse, error) {
	// the token provides it's own authorization and authentication
	return a.authServer.validateTrustedCluster(validateRequest)
}

func (a *AuthWithRoles) DeleteTrustedCluster(name string) error {
	if err := a.action(defaults.Namespace, services.KindTrustedCluster, services.VerbDelete); err != nil {
		return trace.Wrap(err)
	}

	return a.authServer.DeleteTrustedCluster(name)
}

func (a *AuthWithRoles) UpsertTunnelConnection(conn services.TunnelConnection) error {
	if err := a.action(defaults.Namespace, services.KindTunnelConnection, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindTunnelConnection, services.VerbUpdate); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.UpsertTunnelConnection(conn)
}

func (a *AuthWithRoles) GetTunnelConnections(clusterName string) ([]services.TunnelConnection, error) {
	if err := a.action(defaults.Namespace, services.KindTunnelConnection, services.VerbList); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.GetTunnelConnections(clusterName)
}

func (a *AuthWithRoles) GetAllTunnelConnections() ([]services.TunnelConnection, error) {
	if err := a.action(defaults.Namespace, services.KindTunnelConnection, services.VerbList); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.GetAllTunnelConnections()
}

func (a *AuthWithRoles) DeleteTunnelConnection(clusterName string, connName string) error {
	if err := a.action(defaults.Namespace, services.KindTunnelConnection, services.VerbDelete); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.DeleteTunnelConnection(clusterName, connName)
}

func (a *AuthWithRoles) DeleteTunnelConnections(clusterName string) error {
	if err := a.action(defaults.Namespace, services.KindTunnelConnection, services.VerbList); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindTunnelConnection, services.VerbDelete); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.DeleteTunnelConnections(clusterName)
}

func (a *AuthWithRoles) DeleteAllTunnelConnections() error {
	if err := a.action(defaults.Namespace, services.KindTunnelConnection, services.VerbList); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindTunnelConnection, services.VerbDelete); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.DeleteAllTunnelConnections()
}

func (a *AuthWithRoles) CreateRemoteCluster(conn services.RemoteCluster) error {
	if err := a.action(defaults.Namespace, services.KindRemoteCluster, services.VerbCreate); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.CreateRemoteCluster(conn)
}

func (a *AuthWithRoles) GetRemoteCluster(clusterName string) (services.RemoteCluster, error) {
	if err := a.action(defaults.Namespace, services.KindRemoteCluster, services.VerbRead); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.GetRemoteCluster(clusterName)
}

func (a *AuthWithRoles) GetRemoteClusters() ([]services.RemoteCluster, error) {
	if err := a.action(defaults.Namespace, services.KindRemoteCluster, services.VerbList); err != nil {
		return nil, trace.Wrap(err)
	}
	return a.authServer.GetRemoteClusters()
}

func (a *AuthWithRoles) DeleteRemoteCluster(clusterName string) error {
	if err := a.action(defaults.Namespace, services.KindRemoteCluster, services.VerbDelete); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.DeleteRemoteCluster(clusterName)
}

func (a *AuthWithRoles) DeleteAllRemoteClusters() error {
	if err := a.action(defaults.Namespace, services.KindRemoteCluster, services.VerbList); err != nil {
		return trace.Wrap(err)
	}
	if err := a.action(defaults.Namespace, services.KindRemoteCluster, services.VerbDelete); err != nil {
		return trace.Wrap(err)
	}
	return a.authServer.DeleteAllRemoteClusters()
}

func (a *AuthWithRoles) Close() error {
	return a.authServer.Close()
}

func (a *AuthWithRoles) WaitForDelivery(context.Context) error {
	return nil
}

// NewAdminAuthServer returns auth server authorized as admin,
// used for auth server cached access
func NewAdminAuthServer(authServer *AuthServer, sessions session.Service, alog events.IAuditLog) (ClientI, error) {
	ctx, err := NewAdminContext()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return &AuthWithRoles{
		authServer: authServer,
		checker:    ctx.Checker,
		user:       ctx.User,
		alog:       alog,
		sessions:   sessions,
	}, nil
}

// NewAuthWithRoles creates new auth server with access control
func NewAuthWithRoles(authServer *AuthServer,
	checker services.AccessChecker,
	user services.User,
	sessions session.Service,
	alog events.IAuditLog) *AuthWithRoles {
	return &AuthWithRoles{
		authServer: authServer,
		checker:    checker,
		sessions:   sessions,
		user:       user,
		alog:       alog,
	}
}
