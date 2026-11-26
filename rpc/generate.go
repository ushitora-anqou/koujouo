// Package rpc contains the generated Cap'n Proto protocol for the agent.
package rpc

// The sh -c wrapper is required: go:generate parses backquotes as Go raw
// string literals and never passes them to the shell as command substitution.
//go:generate sh -c "capnp compile -I$(go list -m -f {{.Dir}} capnproto.org/go/capnp/v3)/std -ogo koujouo.capnp"
