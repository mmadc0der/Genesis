//go:build !cgo

package main

import "net/http"

func (s *eventServer) handleEventerRoutes(http.ResponseWriter, *http.Request) bool {
	return false
}
