package zeusmaster

import (
	"fmt"
	"math/rand"
	"net"
	"path/filepath"
	"strconv"
	"time"

	"github.com/burke/zeus/go/unixsocket"
)

const zeusSockName string = ".zeus.sock"

func StartClientHandler(tree *ProcessTree, quit chan bool) {
	path, _ := filepath.Abs(zeusSockName)
	addr, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		panic("Can't open socket.")
	}
	listener, err := net.ListenUnix("unix", addr)
	if err != nil {
		ErrorCantCreateListener()
	}
	defer listener.Close()

	connections := make(chan *unixsocket.Usock)
	go func() {
		for {
			if conn, err := listener.AcceptUnix(); err != nil {
				errorUnableToAcceptSocketConnection()
				time.Sleep(500 * time.Millisecond)
			} else {
				connections <- unixsocket.NewUsock(conn)
			}
		}
	}()

	for {
		select {
		case <-quit:
			quit <- true
			return
		case conn := <-connections:
			go handleClientConnection(tree, conn)
		}
	}
}

// see docs/client_master_handshake.md
func handleClientConnection(tree *ProcessTree, usock *unixsocket.Usock) {
	defer usock.Close()

	// we have established first contact to the client.

	// we first read the command and arguments specified from the connection. (step 1)
	msg, err := usock.ReadMessage()
	if err != nil {
		fmt.Println("clienthandler.go:handleClientConnection:read command and arguments:", err)
		return
	}
	command, arguments, err := ParseClientCommandRequestMessage(msg)
	if err != nil {
		fmt.Println("clienthandler.go:handleClientConnection:parse command and arguments:", err)
		return
	}

	commandNode := tree.FindCommand(command)
	if commandNode == nil {
		fmt.Println("ERROR: Node not found!: ", command)
		return
	}
	command = commandNode.Name // resolve aliases
	slaveNode := commandNode.Parent

	// Now we read the terminal IO socket to use for raw IO (step 2)
	clientFd, err := usock.ReadFD()
	if err != nil {
		fmt.Println("Expected FD, none received!")
		return
	}
	fileName := strconv.Itoa(rand.Int())
	clientFile := unixsocket.FdToFile(clientFd, fileName)
	defer clientFile.Close()

	// We now need to fork a new command process.
	// For now, we naively assume it's running...

	if slaveNode.Error != "" {
		// we can skip steps 3-5 as they deal with the command process we're not spawning.
		// Write a fake pid (step 6)
		usock.WriteMessage("0")
		// Write the error message to the terminal
		clientFile.Write([]byte(slaveNode.Error))
		// Skip step 7, and write an exit code to the client (step 8)
		usock.WriteMessage("1")
		return
	}

	// boot a command process and establish a socket connection to it.
	slaveNode.WaitUntilBooted()

	msg = CreateSpawnCommandMessage(command)
	slaveNode.mu.Lock()
	unixsocket.NewUsock(slaveNode.Socket).WriteMessage(msg)
	slaveNode.mu.Unlock()

	// TODO: deadline? how to respond if this is never sent?
	commandFd := <-slaveNode.ClientCommandPTYFileDescriptor
	if err != nil {
		fmt.Println("Couldn't start command process!", err)
	}
	fileName = strconv.Itoa(rand.Int())
	commandFile := unixsocket.FdToFile(commandFd, fileName)
	defer commandFile.Close()

	commandUsock, err := unixsocket.NewUsockFromFile(commandFile)
	if err != nil {
		fmt.Println("MakeUnixSocket", err)
	}
	defer commandUsock.Close()

	// Send the arguments to the command process (step 3)
	commandUsock.WriteMessage(arguments)

	// Send the client terminal connection to the command process (step 4)
	commandUsock.WriteFD(clientFd)

	// Receive the pid from the command process (step 5)
	msg, err = commandUsock.ReadMessage()
	if err != nil {
		fmt.Println(err)
	}
	intPid, _, _ := ParsePidMessage(msg)

	// Send the pid to the client process (step 6)
	strPid := strconv.Itoa(intPid)
	usock.WriteMessage(strPid)

	// Receive the exit status from the command (step 7)
	msg, err = commandUsock.ReadMessage()
	if err != nil {
		fmt.Println("clienthandler.go:handleClientConnection:receive exit status:", err)
		return
	}

	// Forward the exit status to the Client (step 8)
	usock.WriteMessage(msg)

	// Done! Hooray!

}
