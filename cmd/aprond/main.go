// Command aprond is the Apron reference server: package apron, run with the
// flags, config file, and listeners of package aprond. Run aprond --help for
// its flags.
//
// To run it with extensions of your own, write a main package that calls
// aprond.Main with them, as the examples directory of the module does.
package main

import "github.com/apron-chat/apron-server-go/aprond"

func main() { aprond.Main() }
