//go:build !linux

package qualification

import (
	"fmt"
	"os"
)

func openNonblocking(root *os.Root, path string) (*os.File, error) {
	return nil, fmt.Errorf("qualification artifact verification requires Linux")
}
