// Package redisopt turns the service's Redis settings into go-redis options.
package redisopt

import (
	"errors"

	"github.com/redis/go-redis/v9"
)

// Options builds client options from the configuration.
//
// A full URL wins when set (REDIS_URL, for example redis://default:secret@host:6379,
// or rediss:// for TLS). This is what hosted Redis services such as Railway provide.
// Otherwise a plain host:port is used, with an optional password.
func Options(addr, url, password string) (*redis.Options, error) {
	if url != "" {
		opt, err := redis.ParseURL(url)
		if err != nil {
			// Deliberately not wrapping err: parse errors echo the whole URL,
			// and the URL contains the password.
			return nil, errors.New("REDIS_URL is not a valid redis:// or rediss:// URL")
		}
		return opt, nil
	}
	return &redis.Options{Addr: addr, Password: password}, nil
}
