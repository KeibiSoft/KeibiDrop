package tests

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/inconshreveable/log15"
	"github.com/stretchr/testify/require"
)

func TestKeibiDropFlow(t *testing.T) {
	require := require.New(t)
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
		exec.Command("umount", "-f", absAliceMount).Run()
		exec.Command("umount", "-f", absBobMount).Run()
		os.Remove(absAliceMount)
		os.Remove(absBobMount)
	}()

	relayURL := "http://0.0.0.0:54321"
	parsedURL, err := url.Parse(relayURL)
	require.NoError(err, "parse url")

	aliceInPort := 26001
	aliceOutPort := 26002

	bobInPort := 26003
	bobOutPort := 26004

	err = os.Mkdir(absAliceSave, 0777)
	require.NoError(err, "create save dir Alice")

	err = os.Mkdir(absBobSave, 0777)
	require.NoError(err, "create save dir Bob")

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

	aliceFp, err := kdAlice.ExportFingerprint()
	require.NoError(err)

	bobFp, err := kdBob.ExportFingerprint()
	require.NoError(err)

	err = kdAlice.AddPeerFingerprint(bobFp)
	require.NoError(err)

	kdBob.AddPeerFingerprint(aliceFp)
	require.NoError(err)

	ch := make(chan bool)
	go func() {
		ch <- true
		err = kdAlice.CreateRoom()
		require.NoError(err)
	}()

	logger.Info("Sleep a bit for Alice to create room")
	<-ch
	time.Sleep(3 * time.Second)
	logger.Info("Looks ok")

	go func() {
		ch <- true
		err = kdBob.JoinRoom()
		require.NoError(err)
	}()
	logger.Info("Wait a bit for Bob to join")
	<-ch
	logger.Info("sleep 10 sec")
	time.Sleep(10 * time.Second)
	logger.Info("Done")

	file, err := os.Create(absAliceMount + "/ok.txt")
	require.NoError(err)
	require.NotNil(file)

	testString := "Hello secret file sent from Alice"

	_, err = file.Write([]byte(testString))
	require.NoError(err)

	err = file.Close()
	require.NoError(err)

	bobEntr, err := os.ReadDir(absBobMount)
	require.NoError(err)

	require.NotNil(bobEntr)

	require.Equal(len(bobEntr), 1)
	require.Equal("ok.txt", bobEntr[0].Name())

	data, err := os.ReadFile(absBobMount + "/ok.txt")
	require.NoError(err)

	require.Equal(testString, string(data))
}
