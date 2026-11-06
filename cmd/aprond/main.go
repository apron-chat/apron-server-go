// Command aprond is the Apron reference server: package apron, run with the
// flags, config file, and listeners of package aproncmd. Run aprond --help
// for its flags.
//
// To run it with extensions of your own, write a main package that calls
// aproncmd.Main with them, as the examples directory of the module does.
package main

import "github.com/apron-chat/apron-server-go/aproncmd"

func main() { aproncmd.Main() }
