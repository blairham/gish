// Command partool is the hermetic child process for parallel's tests:
// portable stand-ins for echo/sleep/exit, so the suite runs on
// platforms without sh (#88).
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(64)
	}
	switch os.Args[1] {
	case "echo":
		fmt.Println(strings.Join(os.Args[2:], " "))
	case "exit":
		code, _ := strconv.Atoi(os.Args[2]) //nolint:errcheck // bad input exits 0, fine for a fixture
		os.Exit(code)
	case "delay-echo": // sleep n*100ms, then announce — ordering fixture
		n, _ := strconv.Atoi(os.Args[2]) //nolint:errcheck // as above
		time.Sleep(time.Duration(n) * 100 * time.Millisecond)
		fmt.Println("done", os.Args[2])
	case "boom-or-sleep": // fail-fast fixture
		if os.Args[2] == "boom" {
			os.Exit(9)
		}
		time.Sleep(5 * time.Second)
		fmt.Println("survived")
	default:
		os.Exit(64)
	}
}
