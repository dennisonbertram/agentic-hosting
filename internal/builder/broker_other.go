//go:build !linux

package builder

import (
	"context"
	"errors"
	"os"
)

func brokerCall(context.Context, brokerRequest) error {
	return errors.New("build broker requires Linux systemd")
}
func brokerStatus(context.Context, string) (brokerResponse, error) {
	return brokerResponse{}, errors.New("build broker requires Linux systemd")
}
func RunBroker(context.Context, string) error {
	return errors.New("build broker requires Linux systemd")
}
func openWorkerArtifact(string, string, string, int, uint32) (*os.File, error) {
	return nil, errors.New("build worker requires Linux")
}
func setBuildGroup(string) error { return errors.New("build worker requires Linux") }
