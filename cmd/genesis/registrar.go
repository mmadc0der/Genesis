package main

import (
	"context"
	"sync"
)

// grantRegistrar is the provider-driver seam for remote key registration.
// The production GitHub driver is intentionally unsupported. It does not
// resolve an App PEM, mint an installation token, call GitHub, or mutate
// a remote. A fake driver is injected only by tests.
type grantRegistrar interface {
	Register(ctx context.Context, req grantRegistration) (grantRegistrationResult, error)
}

type grantRegistration struct {
	GrantID     string
	Agent       string
	Repository  string
	Fingerprint string
	PublicKey   string
}

type grantRegistrationResult struct {
	Status      string
	RemoteKeyID string
}

type githubRegistrar struct{}

func (githubRegistrar) Register(ctx context.Context, _ grantRegistration) (grantRegistrationResult, error) {
	if err := ctx.Err(); err != nil {
		return grantRegistrationResult{}, err
	}
	return grantRegistrationResult{Status: remoteRegistrationUnsupported}, nil
}

// fakeRegistrar lets tests complete the ready path without a network.
type fakeRegistrar struct {
	mu     sync.Mutex
	Status string
	KeyID  string
	Err    error
	Calls  int
	Last   grantRegistration
}

func (f *fakeRegistrar) Register(_ context.Context, req grantRegistration) (grantRegistrationResult, error) {
	if f == nil {
		return grantRegistrationResult{Status: remoteRegistrationUnsupported}, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	f.Last = req
	if f.Err != nil {
		return grantRegistrationResult{}, f.Err
	}
	status := f.Status
	if status == "" {
		status = remoteRegistrationUnsupported
	}
	switch status {
	case remoteRegistrationUnsupported, remoteRegistrationNone, remoteStatusReady, remoteStatusPending:
	default:
		return grantRegistrationResult{}, errUnknownRegistrationStatus
	}
	return grantRegistrationResult{Status: status, RemoteKeyID: f.KeyID}, nil
}
