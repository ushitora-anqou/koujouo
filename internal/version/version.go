/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package version holds the build identity the manager and the agent of
// one controller image share.
package version

// Agent is the version of the agent binary of this build. The Nix flake
// and the Makefile inject the short git revision of the source tree with
// -X at build time; a binary built without the injection reports "dev".
//
// The agent reports it in its status and the manager compares it with
// its own value: an agent of another version receives the agent binary
// of the manager image through an upgrade.
//
//nolint:gochecknoglobals // The build injects the version with -X, which requires a package-level variable.
var Agent = "dev"
