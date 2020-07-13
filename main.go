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
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/ChainSafe/gossamer/lib/common"
	"github.com/ChainSafe/gossamer/lib/common/optional"
	"github.com/ChainSafe/gossamer/lib/runtime/extrinsic"
)

var keys = []string{"alice", "bob", "charlie", "dave", "eve", "fred", "george", "heather", "ian"}
var genesis = "genesis.json"
var config = "config.toml"

var (
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

func initAndStart(idx int, outfile, errfile *os.File) *exec.Cmd {
	basepath := "~/.gossamer_" + keys[idx]

	initCmd := exec.Command("../../ChainSafe/gossamer/bin/gossamer",
		"init",
		"--config", config,
		"--basepath", basepath,
		"--genesis", genesis,
		"--force",
	)

	// init gossamer
	stdout, err := initCmd.CombinedOutput()
	if err != nil {
		panic(err)
	}

	outfile.Write(stdout)
	fmt.Println("initialized node", keys[idx])

	gssmrCmd := exec.Command("../../ChainSafe/gossamer/bin/gossamer",
		"--port", strconv.Itoa(7000+idx),
		"--config", config,
		"--key", keys[idx],
		"--basepath", basepath,
		"--rpcport", strconv.Itoa(8540+idx),
		"--rpc",
	)

	stdoutPipe, err := gssmrCmd.StdoutPipe()
	if err != nil {
		panic(err)
	}

	stderrPipe, err := gssmrCmd.StderrPipe()
	if err != nil {
		panic(err)
	}

	err = gssmrCmd.Start()
	if err != nil {
		panic(err)
	}

	writer := bufio.NewWriter(outfile)
	go io.Copy(writer, stdoutPipe)
	errWriter := bufio.NewWriter(errfile)
	go io.Copy(errWriter, stderrPipe)
	return gssmrCmd
}

func main() {
	baseport := 8540
	num := 3
	var err error

	if len(os.Args) > 1 {
		num, err = strconv.Atoi(os.Args[1])
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		if num%3 != 0 {
			fmt.Print("must do 3, 6, 9 nodes")
			os.Exit(1)
		}
	}

	fmt.Println("num nodes:", num)

	// initialize and start nodes
	processes := []*exec.Cmd{}

	var wg sync.WaitGroup
	wg.Add(num)
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
		defer outfile.Close()

		go func(i int, outfile *os.File) {
			p := initAndStart(i, outfile, errfile)
			processes = append(processes, p)
			wg.Done()
		}(i, outfile)
	}
	wg.Wait()

	defer func() {
		for i := 0; i < num; i++ {
			err = processes[i].Process.Kill()
			if err != nil {
				//fmt.Printf("could not kill process %s!!! %s\n", keys[i], err)
			}
		}
	}()

	// wait for node to start
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
			res, err = getStorage("http://localhost:"+strconv.Itoa(baseport+i), key)
			if err == nil {
				break
			}

			time.Sleep(time.Second)
		}

		fmt.Printf("got storage from node %d: 0x%x\n", i, res)
	}

	// submit extrinsic
	respBody, err := postRPC("author_submitExtrinsic", "http://localhost:"+strconv.Itoa(baseport), "\"0x"+txStr+"\"")
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	fmt.Println("submitted extrinsic")
	fmt.Printf("response: %s\n", respBody)

	// query for storage
	for i := 0; i < num; i++ {
		wg.Add(1)

		go func(i int) {
			var res []byte
			for j := 0; j < maxRetries; j++ {
				res, err = getStorage("http://localhost:"+strconv.Itoa(baseport+i), key)
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
}
