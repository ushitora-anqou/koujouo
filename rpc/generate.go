// Package rpc contains the generated Cap'n Proto protocol for the agent.
package rpc

// The sh -c wrapper is required: go:generate parses backquotes as Go raw
// string literals and never passes them to the shell as command substitution,
// and it runs the command without a shell, so $(go list ...) stays literal
// otherwise.
//
// The capnp compiler writes the CodeGeneratorRequest to its standard output
// (-o-) and go tool capnpc-go reads it from there. capnpc-go is the tool of
// go.mod (see the tool directive), not a package the Nix flake builds.
//go:generate sh -c "capnp compile -I$(go list -m -f {{.Dir}} capnproto.org/go/capnp/v3)/std -o- koujouo.capnp | go tool capnpc-go"
