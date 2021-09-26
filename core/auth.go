package core

import log "github.com/sirupsen/logrus"

// Authenticator is an interface for user authentication.
type Authenticator interface {
	Authenticate(user, password string) bool
}

var DefaultAuthenticator = &UsernamePassword{}

// UsernamePassword are the credentials for the username/password authentication method.
type UsernamePassword struct {
	Username string
	Password string
}

// Authenticate authenticates a pair of username and password with the proxy server.
func (up *UsernamePassword) Authenticate(username, password string) bool {
	log.Infof("username: %s, password: %s\n", username, password)
	return true
}
