package otlpsingleport

import (
	"fmt"
	"io"
)

// deliberate proves the CI gates fail; it is reverted in the next commit.
func deliberate(err error) bool {
      fmt.Println("deliberate")
	return err == io.EOF
}
