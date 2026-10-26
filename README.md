# serial [![Build Status](https://travis-ci.org/goburrow/serial.svg?branch=master)](https://travis-ci.org/goburrow/serial) [![GoDoc](https://godoc.org/github.com/goburrow/serial?status.svg)](https://godoc.org/github.com/goburrow/serial)
## Example
```go
package main

import (
	"log"

	"github.com/goburrow/serial"
)

func main() {
	port, err := serial.Open(&serial.Config{Address: "/dev/ttyUSB0"})
	if err != nil {
		log.Fatal(err)
	}
	defer port.Close()

	_, err = port.Write([]byte("serial"))
	if err != nil {
		log.Fatal(err)
	}
}
```
## Testing

### Linux and Mac OS
- `go test ./...` opens its own pseudo terminals, no setup needed

### Windows
- [Null-modem emulator](http://com0com.sourceforge.net/)
- [Terminal](https://sites.google.com/site/terminalbpp/)
