package api

import "net/http"

// Deps is what the API serves from.
type Deps struct{}

// Server is the node's listeners.
type Server struct{}

// New returns the server.
func New(Deps) *Server { return &Server{} }

// Start starts the server.
func (*Server) Start() error { return nil }

// Stop stops the server.
func (*Server) Stop() error { return nil }

// Handler exports what the api must not.
func (*Server) Handler() http.Handler { return nil }
