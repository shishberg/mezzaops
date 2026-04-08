package service

import "context"

// Backend manages the lifecycle of a service.
type Backend interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Restart(ctx context.Context) error
	Status(ctx context.Context) (string, error)
	Logs(ctx context.Context, tail int) (string, error)
}
