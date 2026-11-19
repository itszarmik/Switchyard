package harness

import (
	"fmt"
	"os"
)

// Signal sends sig to the gateway process, for example SIGHUP to reload
// its configuration.
func (g *Gateway) Signal(sig os.Signal) error {
	if err := g.cmd.Process.Signal(sig); err != nil {
		return fmt.Errorf("harness: sending %s: %w", sig, err)
	}
	return nil
}
