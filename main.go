// Command plugin is the ClickHouse plugin for Līdza Deploy.
//
// It knows what the deploy agent should not have to: which ClickHouse
// releases a server's processor can run, whether the server on this host is
// healthy, whether the data that is about to arrive fits where ClickHouse
// keeps it, and how to bring a failed server back.
//
// This version implements the hooks that look at and repair a server:
// describe, check, status and repair. Creating databases, backups and moves
// are still done by the agent's built-in ClickHouse support.
package main

import (
	"context"

	"github.com/agim/lidza-deploy/pluginapi"
)

func main() {
	h := realHost{}
	pluginapi.Main(map[string]pluginapi.Handler{
		"describe": func(context.Context, pluginapi.Request) (pluginapi.Answer, error) {
			return pluginapi.Answer{OK: true, Messages: []string{"Checks and repairs the ClickHouse server of this host."}}, nil
		},
		"check":  func(ctx context.Context, req pluginapi.Request) (pluginapi.Answer, error) { return check(ctx, h, req) },
		"status": func(ctx context.Context, req pluginapi.Request) (pluginapi.Answer, error) { return status(ctx, h) },
		"repair": func(ctx context.Context, req pluginapi.Request) (pluginapi.Answer, error) { return repair(ctx, h) },
	})
}
