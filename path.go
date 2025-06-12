package gopv

import (
	"fmt"
	"math/rand"
	"os"
)

// Generates a suitable path for an mpv socket.
func GeneratePath() (string, error) {
	ipcDir := os.TempDir() + "/mpvsockets"
	err := os.MkdirAll(ipcDir, os.ModePerm)
	if err != nil {
		return "", err
	}
	
	return fmt.Sprintf("%s/%d", ipcDir, rand.Uint32()), nil
}
