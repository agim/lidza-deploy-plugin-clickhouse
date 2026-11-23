// Command plugin is the ClickHouse plugin for Līdza Deploy.
//
// It knows what the deploy agent should not have to: which ClickHouse
// releases a server's processor can run, whether the server on this host is
// healthy, whether the data that is about to arrive fits where ClickHouse
// keeps it, and how to bring a failed server back.
//
// It also runs the ClickHouse of a host for the applications on it: it
// installs the server from ClickHouse's own package repository, gives each
// application a database, a user and a settings profile of its own, backs
// that database up and restores it.
package main

import (
	"context"

	"github.com/agim/lidza-deploy/pluginapi"
)

func main() {
	h := realHost{}
	m := newMachine()
	pluginapi.Main(map[string]pluginapi.Handler{
		"describe": func(context.Context, pluginapi.Request) (pluginapi.Answer, error) {
			return pluginapi.Answer{OK: true, Messages: []string{"Runs the ClickHouse server of this host for the applications on it."}}, nil
		},
		"check":  func(ctx context.Context, req pluginapi.Request) (pluginapi.Answer, error) { return check(ctx, h, req) },
		"status": func(ctx context.Context, req pluginapi.Request) (pluginapi.Answer, error) { return status(ctx, h) },
		"repair": func(ctx context.Context, req pluginapi.Request) (pluginapi.Answer, error) { return repair(ctx, h) },
		"provision": func(ctx context.Context, req pluginapi.Request) (pluginapi.Answer, error) {
			return provision(ctx, m, req)
		},
		"release": func(ctx context.Context, req pluginapi.Request) (pluginapi.Answer, error) {
			return release(ctx, m, req)
		},
		"backup": func(ctx context.Context, req pluginapi.Request) (pluginapi.Answer, error) { return backup(ctx, m, req) },
		"restore": func(ctx context.Context, req pluginapi.Request) (pluginapi.Answer, error) {
			return restore(ctx, m, req)
		},
		"maintain": func(ctx context.Context, req pluginapi.Request) (pluginapi.Answer, error) {
			return maintain(ctx, m, req)
		},
	})
}
