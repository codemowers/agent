package main

import (
	"net/url"
	"testing"
)

func TestBuildAMQPURLEscapesCredentialsAndVhost(t *testing.T) {
	t.Setenv("AMQP_URL", "")
	t.Setenv("AMQP_HOST", "rabbitmq")
	t.Setenv("AMQP_USERNAME", "user@host")
	t.Setenv("AMQP_PASSWORD", "p/a:s@word")
	t.Setenv("AMQP_VHOST", "/")
	u, err := url.Parse(buildAMQPURL())
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	if u.User.Username() != "user@host" || password != "p/a:s@word" || u.EscapedPath() != "/%2F" {
		t.Fatalf("invalid broker URL encoding: %s", u.Redacted())
	}
}
