package tests

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/inconshreveable/log15"
	"github.com/stretchr/testify/require"
)

func TestKeibiDropFlow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	absPath := "/Users/marius/work/code/KeibiDrop/tests"
	absAliceSave := absPath + "/SaveAlice"
	absBobSave := absPath + "/SaveBob"

	absAliceMount := absPath + "/MountAlice"
	absBobMount := absPath + "/MountBob"

	defer func() {
		os.Remove(absAliceSave)
		os.Remove(absBobSave)
	}()

	relayURL := "http://0.0.0.0:54321"
	parsedURL, err := url.Parse(relayURL)
	require.NoError(t, err, "parse url")

	aliceInPort := 26001
	aliceOutPort := 26002

	bobInPort := 26003
	bobOutPort := 26004

	err = os.Mkdir(absAliceSave, 0777)
	require.NoError(t, err, "create save dir Alice")

	err = os.Mkdir(absBobSave, 0777)
	require.NoError(t, err, "create save dir Bob")

	logger := log15.New("method", "test")

	kdAlice, err := common.NewKeibiDrop(ctx, logger, parsedURL, aliceInPort, aliceOutPort, absAliceMount, absAliceSave)
	if err != nil {
		logger.Error("Failed to start keibidrop", "error", err)
		os.Exit(1)
	}

	go kdAlice.Run()

	kdBob, err := common.NewKeibiDrop(ctx, logger, parsedURL, bobInPort, bobOutPort, absBobMount, absBobSave)
	if err != nil {
		logger.Error("Failed to start keibidrop", "error", err)
		os.Exit(1)
	}

	go kdBob.Run()

}
