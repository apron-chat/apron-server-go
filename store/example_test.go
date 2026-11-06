package store_test

import (
	"fmt"
	"log"

	"github.com/apron-chat/apron-server-go/store"
)

// Open names a backend as aprond's --store flag does. A store is given to the
// server as apron.Config.Store, which closes it on Shutdown.
func ExampleOpen() {
	s, err := store.Open("memory")
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	if err := s.Apply([]store.Entry{{Kind: "note", ID: "1", Value: []byte(`"hello"`)}}); err != nil {
		log.Fatal(err)
	}
	s.Load(func(e store.Entry) error {
		fmt.Printf("%s/%s = %s\n", e.Kind, e.ID, e.Value)
		return nil
	})
	// Output: note/1 = "hello"
}
