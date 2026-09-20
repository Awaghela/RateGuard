package redisopt

import (
	"strings"
	"testing"
)

func TestURLCarriesHostUserAndPassword(t *testing.T) {
	o, err := Options("ignored:1", "redis://default:s3cret@redis.railway.internal:6379", "ignored")
	if err != nil {
		t.Fatal(err)
	}
	if o.Addr != "redis.railway.internal:6379" || o.Username != "default" || o.Password != "s3cret" {
		t.Fatalf("unexpected options: addr=%q user=%q pass=%q", o.Addr, o.Username, o.Password)
	}
}

func TestURLWinsOverAddrAndPassword(t *testing.T) {
	o, err := Options("localhost:6379", "redis://:fromurl@example:6380", "fromenv")
	if err != nil {
		t.Fatal(err)
	}
	if o.Addr != "example:6380" || o.Password != "fromurl" {
		t.Fatalf("URL should win: addr=%q pass=%q", o.Addr, o.Password)
	}
}

func TestPlainAddrKeepsWorkingWithoutPassword(t *testing.T) {
	o, err := Options("redis:6379", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if o.Addr != "redis:6379" || o.Password != "" || o.TLSConfig != nil {
		t.Fatalf("unexpected options: %+v", o)
	}
}

func TestAddrWithSeparatePassword(t *testing.T) {
	o, err := Options("redis:6379", "", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if o.Addr != "redis:6379" || o.Password != "pw" {
		t.Fatalf("unexpected options: %+v", o)
	}
}

func TestRedissSchemeTurnsOnTLS(t *testing.T) {
	o, err := Options("", "rediss://default:pw@secure.example:6380", "")
	if err != nil {
		t.Fatal(err)
	}
	if o.TLSConfig == nil {
		t.Fatal("rediss:// should enable TLS")
	}
}

func TestBadURLErrorDoesNotLeakThePassword(t *testing.T) {
	for _, bad := range []string{
		"redis://default:s3cret@host:notaport",
		"http://default:s3cret@host:6379",
	} {
		_, err := Options("", bad, "")
		if err == nil {
			t.Fatalf("expected an error for %q", bad)
		}
		if strings.Contains(err.Error(), "s3cret") {
			t.Fatalf("error leaks the password: %v", err)
		}
	}
}
