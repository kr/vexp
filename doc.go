/*

Command vexp is a vendoring experiment.

Usage

	vexp [-v] [-u packages]

Vexp finds all dependencies of all packages in ./...,
and copies their files into subdirectory "vendor", such
that the go tool will use the copied packages when run
with GO15VENDOREXPERIMENT=1 in its environment.

For more details on the Go 1.5 vendor experiment, see
https://groups.google.com/d/msg/golang-dev/74zjMON9glU/4lWCRDCRZg0J

With no options, vexp only adds new packages; existing
packages are left unchanged.

Flag -u updates already-vendored dependencies. It takes
a colon-separated list of package patterns. If any
dependency matches one of these patterns, it will be
copied from $GOPATH into the vendor directory, even if
already present.

For more about specifying packages, see 'go help packages'.

*/
package main
