package main

type Authenticator interface {
	Auth(username, password string) bool
}
