package hysteria

import (
	"context"
	"testing"
)

func TestRetiredClientCannotReconnect(t *testing.T) {
	c := &client{}
	c.clean(true)
	if _, err := c.tcp(context.Background()); err == nil {
		t.Fatal("retired TCP client attempted a new dial")
	}
	if _, err := c.udp(context.Background()); err == nil {
		t.Fatal("retired UDP client attempted a new dial")
	}
}
