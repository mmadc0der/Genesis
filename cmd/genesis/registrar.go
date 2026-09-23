package main

import (
	"context"
	"sync"
)

// grantRegistrar is the provider-driver seam for remote key registration.
// githubRegistrar performs that call when a transport and secret resolver
// are set. The zero value does not resolve an App PEM, mint an installation
// token, call GitHub, or mutate a remote. Tests inject fakeRegistrar.
type grantRegistrar interface {
	Register(ctx context.Context, req grantRegistration) (grantRegistrationResult, error)
}

type grantRegistration struct {
	GrantID     string
	Agent       string
	Repository  string
	Identity    string
	Fingerprint string
	PublicKey   string
	Git         string
	Permissions map[string]string
	RemoteKeyID string
}

type grantRegistrationResult struct {
	Status      string
	RemoteKeyID string
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
	case remoteRegistrationUnsupported, remoteRegistrationNone, remoteStatusReady, remoteStatusPending, remoteStatusRefused:
	default:
		return grantRegistrationResult{}, errUnknownRegistrationStatus
	}
	return grantRegistrationResult{Status: status, RemoteKeyID: f.KeyID}, nil
}
