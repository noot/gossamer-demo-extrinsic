package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/ChainSafe/gossamer/dot/rpc/modules"
	"github.com/ChainSafe/gossamer/lib/common"
	"github.com/ChainSafe/gossamer/lib/common/optional"
	"github.com/ChainSafe/gossamer/lib/runtime/extrinsic"
	"github.com/urfave/cli"
)

var (
	numFlag = cli.UintFlag{
		Name:  "num",
		Usage: "number of nodes",
	}

	connectFlag = cli.BoolFlag{
		Name:  "connect",
		Usage: "directly connect nodes",
	}

	pathFlag = cli.StringFlag{
		Name:  "path",
		Usage: "path to gossamer binary",
	}
)

var flags = []cli.Flag{
	numFlag,
	connectFlag,
	pathFlag,
}

var (
	app          = cli.NewApp()
	gossamerPath = "../../ChainSafe/gossamer/bin/gossamer"
	baseaddr     = "/ip4/127.0.0.1/tcp/"

	keys        = []string{"alice", "bob", "charlie", "dave", "eve", "fred", "george", "heather", "ian"}
	baseRPCPort = 8540
	baseport    = 7000
	genesis     = "genesis.json"
	config      = "config.toml"

	maxRetries        = 36
	httpClientTimeout = 120 * time.Second
	dialTimeout       = 60 * time.Second

	transport = &http.Transport{
		Dial: (&net.Dialer{
			Timeout: dialTimeout,
		}).Dial,
	}
	httpClient = &http.Client{
		Transport: transport,
		Timeout:   httpClientTimeout,
	}
)

// ServerResponse wraps the RPC response
type ServerResponse struct {
	// JSON-RPC Version
	Version string `json:"jsonrpc"`
	// Resulting values
	Result json.RawMessage `json:"result"`
	// Any generated errors
	Error *Error `json:"error"`
	// Request id
	ID *json.RawMessage `json:"id"`
}

// ErrCode is a int type used for the rpc error codes
type ErrCode int

// Error is a struct that holds the error message and the error code for a error
type Error struct {
	Message   string                 `json:"message"`
	ErrorCode ErrCode                `json:"code"`
	Data      map[string]interface{} `json:"data"`
}

func postRPC(method, host, params string) ([]byte, error) {
	data := []byte(`{"jsonrpc":"2.0","method":"` + method + `","params":` + params + `,"id":1}`)
	buf := &bytes.Buffer{}
	_, err := buf.Write(data)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	r, err := http.NewRequest("POST", host, buf)
	if err != nil {
		return nil, err
	}

	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(r)
	if err != nil {
		return nil, err
	} else if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status code not OK")
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	return respBody, nil
}

func decodeRPC(body []byte, target interface{}) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()

	var response ServerResponse
	err := decoder.Decode(&response)
	if err != nil {
		return err
	}

	if response.Error != nil {
		return errors.New(response.Error.Message)
	}

	decoder = json.NewDecoder(bytes.NewReader(response.Result))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func getStorage(endpoint string, key []byte) ([]byte, error) {
	respBody, err := postRPC("state_getStorage", endpoint, "[\""+common.BytesToHex(key)+"\"]")
	if err != nil {
		return nil, err
	}

	v := new(string)
	err = decodeRPC(respBody, v)
	if err != nil {
		return nil, err
	}

	if *v == "" {
		return []byte{}, nil
	}

	value, err := common.HexToBytes(*v)
	if err != nil {
		return nil, err
	}

	return value, nil
}

func initAndStart(idx int, genesis, bootnodes string, outfile, errfile *os.File) *exec.Cmd {
	basepath := "~/.gossamer_" + keys[idx]

	initCmd := exec.Command(gossamerPath,
		"init",
		"--config", config,
		"--basepath", basepath,
		"--genesis", genesis,
		"--force",
	)

	// init gossamer
	stdout, err := initCmd.CombinedOutput()
	if err != nil {
		fmt.Printf("failed to initialize node %d: %s\n", idx, err)
		os.Exit(1)
	}

	outfile.Write(stdout)
	fmt.Println("initialized node", keys[idx])

	gssmrCmd := exec.Command(gossamerPath,
		"--port", strconv.Itoa(baseport+idx),
		"--config", config,
		"--key", keys[idx],
		"--basepath", basepath,
		"--rpcport", strconv.Itoa(baseRPCPort+idx),
		"--rpc",
		"--bootnodes", bootnodes,
	)

	stdoutPipe, err := gssmrCmd.StdoutPipe()
	if err != nil {
		fmt.Printf("failed to get stdoutPipe from node %d: %s\n", idx, err)
		os.Exit(1)
	}

	stderrPipe, err := gssmrCmd.StderrPipe()
	if err != nil {
		fmt.Printf("failed to get stderrPipe from node %d: %s\n", idx, err)
		os.Exit(1)
	}

	err = gssmrCmd.Start()
	if err != nil {
		fmt.Printf("failed to start node %d: %s\n", idx, err)
		os.Exit(1)
	}

	writer := bufio.NewWriter(outfile)
	go io.Copy(writer, stdoutPipe)
	errWriter := bufio.NewWriter(errfile)
	go io.Copy(errWriter, stderrPipe)
	return gssmrCmd
}

func getPeerID(endpoint string) (string, error) {
	respBody, err := postRPC("system_networkState", endpoint, "[]")
	if err != nil {
		return "", err
	}

	networkState := new(modules.SystemNetworkStateResponse)
	err = decodeRPC(respBody, &networkState)
	if err != nil {
		return "", err
	}

	return networkState.NetworkState.PeerID, nil
}

func init() {
	app.Action = run
	app.Flags = flags
}

func main() {
	if err := app.Run(os.Args); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx *cli.Context) error {
	var err error

	num := int(ctx.Uint(numFlag.Name))
	if num%3 != 0 {
		fmt.Print("must do 3, 6, 9 nodes")
		os.Exit(1)
	}

	connect := ctx.Bool(connectFlag.Name)
	path := ctx.String(pathFlag.Name)
	if path != "" {
		gossamerPath = path
	}

	fmt.Println("num nodes:", num)

	// initialize and start nodes
	processes := []*exec.Cmd{}

	var wg sync.WaitGroup
	wg.Add(num)
	var nodeAddr string // used for directly connecting nodes

	for i := 0; i < num; i++ {
		outfile, err := os.Create("./log_" + keys[i] + ".out")
		if err != nil {
			panic(err)
		}
		defer outfile.Close()

		errfile, err := os.Create("./err_" + keys[i] + ".out")
		if err != nil {
			panic(err)
		}
		defer errfile.Close()

		if connect && i == 0 {
			// all other nodes will directly connect to the first node
			// the other nodes are able to discover each other through the connection to the first node
			// as well as mDNS
			p := initAndStart(i, genesis, "", outfile, errfile)
			processes = append(processes, p)
			wg.Done()

			var peerID string
			for j := 0; j < maxRetries; j++ {
				peerID, err = getPeerID("http://localhost:" + strconv.Itoa(baseRPCPort))
				if err == nil {
					break
				}
				time.Sleep(time.Second)
			}

			if err != nil {
				fmt.Println("failed to get peerID from first node")
				return err
			}

			nodeAddr = baseaddr + strconv.Itoa(baseport) + "/p2p/" + peerID
			fmt.Println("got node addr for node", nodeAddr)
			continue
		}

		go func(i int, outfile *os.File) {
			p := initAndStart(i, genesis, nodeAddr, outfile, errfile)
			processes = append(processes, p)
			wg.Done()
		}(i, outfile)
	}
	wg.Wait()

	for i := 0; i < num; i++ {
		go func(i int) {
			err = processes[i].Wait()
			if err != nil {
				fmt.Printf("process %s failed!!! %s\n", keys[i], err)
			}
		}(i)
	}

	defer func() {
		for i := 0; i < num; i++ {
			err = processes[i].Process.Kill()
			if err != nil {
				fmt.Printf("could not kill process %s!!! %s\n", keys[i], err)
			}
		}
	}()

	// wait for nodes to start
	time.Sleep(time.Second * 5)

	// create StorageChange extrinsic
	key := []byte("noot")
	value := []byte("washere")
	ext := extrinsic.NewStorageChangeExt(key, optional.NewBytes(true, value))
	tx, err := ext.Encode()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	txStr := hex.EncodeToString(tx)

	// get storage before
	fmt.Println("storage before")
	for i := 0; i < num; i++ {
		var res []byte
		for j := 0; j < 8; j++ {
			res, err = getStorage("http://localhost:"+strconv.Itoa(baseRPCPort+i), key)
			if err == nil {
				break
			}

			time.Sleep(time.Second)
		}

		fmt.Printf("got storage from node %d: 0x%x\n", i, res)
	}

	// submit extrinsic
	r := rand.Intn(num)
	respBody, err := postRPC("author_submitExtrinsic", "http://localhost:"+strconv.Itoa(baseRPCPort+r), "\"0x"+txStr+"\"")
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	fmt.Printf("submitted extrinsic to node %d\n", r)
	fmt.Printf("response: %s\n", respBody)

	// query for storage
	for i := 0; i < num; i++ {
		wg.Add(1)

		go func(i int) {
			var res []byte
			for j := 0; j < maxRetries; j++ {
				res, err = getStorage("http://localhost:"+strconv.Itoa(baseRPCPort+i), key)
				if err == nil && !bytes.Equal(res, []byte{}) {
					break
				}

				time.Sleep(time.Second)
			}

			fmt.Printf("got storage from node %d: 0x%x\n", i, res)
			wg.Done()
		}(i)
	}

	wg.Wait()
	return nil
}
