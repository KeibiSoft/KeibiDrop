package transport

import (
	"fmt"
	"os"
)

// debugOn gates the intermediary trace prints. Enable with QUICDBG=1 to follow
// connection teardown ordering when diagnosing hangs; off by default so normal
// runs stay quiet.
var debugOn = os.Getenv("QUICDBG") != ""

func dbg(format string, a ...any) {
	if debugOn {
		fmt.Printf("[dbg] "+format+"\n", a...)
	}
}
