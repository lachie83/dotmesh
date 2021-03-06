package main

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os/exec"
	"strings"

	"github.com/gorilla/mux"
)

// machinery for remote zfs replication

const BUF_LEN = 131072         // 128kb of replication data sent per update (of etcd)
const START_SNAPSHOT = "START" // meaning "the start of the filesystem"

func (z ZFSSender) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// respond to GET requests with a ZFS data stream
	vars := mux.Vars(r)
	z.fromSnap = vars["fromSnap"]
	z.toSnap = vars["toSnap"]
	z.filesystem = vars["filesystem"]

	// TODO: add a coarse grained lock to start with: stop other readers from
	// this filesystem, and also stop us moving this filesystem to another node
	// while it's being read from (although maybe avoid cancelling
	// inter-cluster replications, and instead block migrations on those)
	//
	// later, implement extent-based locking (ie, this read locks snapshots
	// B-G, but allow A or H to be deleted)

	// z.state.lockFilesystem(z.filesystem)
	// defer z.state.unlockFilesystem(z.filesystem)

	master := z.state.masterFor(z.filesystem)

	if master != z.state.myNodeId {
		addresses := z.state.addressesFor(master)
		// XXX hack, IPv4 happens to come before IPv6 and happens to be routeable
		// on my network (whereas IPv6 isn't), but this depends on the enumeration
		// order of network cards in servers :/
		// TODO we should really attempt each address in turn until we find one
		// that works.
		peerAddress := addresses[0]

		url := fmt.Sprintf(
			"%s/filesystems/%s/%s/%s",
			deduceUrl(peerAddress, "internal"), // FIXME, need master->name mapping, see how handover works normally
			z.filesystem,
			z.fromSnap,
			z.toSnap,
		)

		// Proxy request to the master
		req, err := http.NewRequest(
			"GET", url,
			r.Body,
		)

		_, _, apiKey, err := getPasswords("admin")
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(fmt.Sprintf("Can't establish API key to proxy pull: %+v.\n", err)))
			return
		}

		req.SetBasicAuth(
			"admin",
			apiKey,
		)
		postClient := new(http.Client)
		log.Printf("[ZFSSender:ServeHTTP] Proxying pull from %s: %s", master, url)
		resp, err := postClient.Do(req)
		finished := make(chan bool)
		log.Printf("[ZFSSender:ServeHTTP] Got HTTP response +v", resp.StatusCode)
		w.WriteHeader(resp.StatusCode)
		go pipe(resp.Body, url,
			w, "proxied pull recipient",
			finished,
			make(chan *Event),
			func(e *Event, c chan *Event) {},
			func(bytes int64, t int64) {},
			"none",
		)
		defer resp.Body.Close()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(fmt.Sprintf("Can't proxy pull from %s: %+v.\n", url, err)))
			return
		}
		log.Printf("[ZFSSender:ServeHTTP] Waiting for finish signal...")
		_ = <-finished
		return
	}

	log.Printf(
		"[ZFSSender:ServeHTTP] Starting to send replication stream for %s from %s => %s",
		z.filesystem, z.fromSnap, z.toSnap,
	)

	// command writes into pipe
	var cmd *exec.Cmd

	prelude, err := z.state.calculatePrelude(z.filesystem, z.toSnap)
	if err != nil {
		log.Printf(
			"[ZFSSender:ServeHTTP] Error calculating prelude in from zfs send of %s from %s => %s: %s",
			z.filesystem, z.fromSnap, z.toSnap, err,
		)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf("Unable to calculate prelude: %s\n", err)))
		return
	}

	if z.fromSnap == "START" {
		cmd = exec.Command(
			// -R sends interim snapshots as well
			"zfs", "send", "-p", "-R", fq(z.filesystem)+"@"+z.toSnap,
		)
	} else {
		var fromSnap string
		// in clone case, z.fromSnap must be fully qualified
		if strings.Contains(z.fromSnap, "@") {
			// send a clone, so make it fully qualified
			fromSnap = fq(z.fromSnap)
		} else {
			// presume it refers to a snapshot
			fromSnap = z.fromSnap
		}
		cmd = exec.Command(
			"zfs", "send", "-p",
			"-I", fromSnap, fq(z.filesystem)+"@"+z.toSnap,
		)
	}

	// How to set HTTP response code based on return code of process?
	// (we can't - it's too late by the time we know the return code)
	pipeReader, pipeWriter := io.Pipe()

	defer pipeWriter.Close()
	defer pipeReader.Close()

	preludeEncoded, err := encodePrelude(prelude)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf("Can't encode prelude: %s\n", err)))
		return
	}

	cmd.Stdout = pipeWriter
	cmd.Stderr = getLogfile("zfs-send-errors")

	finished := make(chan bool)
	go pipe(
		pipeReader, fmt.Sprintf("stdout of zfs send for %s", z.filesystem),
		w, "http response body",
		finished,
		make(chan *Event),
		func(e *Event, c chan *Event) {},
		func(bytes int64, t int64) {},
		"compress",
	)

	log.Printf(
		"[ZFSSender:ServeHTTP] Writing prelude of %d bytes (encoded): %s",
		len(preludeEncoded), preludeEncoded,
	)
	pipeWriter.Write(preludeEncoded)

	log.Printf(
		"[ZFSSender:ServeHTTP] About to Run() for %s %s => %s",
		z.filesystem, z.fromSnap, z.toSnap,
	)
	err = cmd.Run()
	log.Printf(
		"[ZFSSender:ServeHTTP] Finished Run() for %s %s => %s: %s",
		z.filesystem, z.fromSnap, z.toSnap, err,
	)
	if err != nil {
		log.Printf(
			"[ZFSSender:ServeHTTP] Error from zfs send of %s from %s => %s: %s, check zfs-send-errors.log",
			z.filesystem, z.fromSnap, z.toSnap, err,
		)
	}
	// XXX Adding the log messages below seemed to stop a deadlock, not sure
	// why. For now, let's just leave them in...
	log.Printf("[ZFSSender:ServeHTTP] Closing pipes...")
	pipeWriter.Close()
	pipeReader.Close()

	log.Printf("[ZFSSender:ServeHTTP] Waiting for finish signal...")
	_ = <-finished
	log.Printf("[ZFSSender:ServeHTTP] Done!")

}

// POST request => zfs recv command (only used by recipient of "dm push", ie pushPeerState)
func (z ZFSReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	log.Printf("[ZFSReceiver:ServeHTTP] r: %v vars: %v", r, vars)
	z.fromSnap = vars["fromSnap"]
	z.toSnap = vars["toSnap"]
	z.filesystem = vars["filesystem"]
	_ = make([]byte, BUF_LEN)

	// TODO: add a coarse grained lock to start with: stop other writers from
	// writing to this filesystem (unlike readers, this is strictly
	// one-at-a-time), and also stop us moving this filesystem to another node
	// while it's being written to
	// z.state.lockFilesystem(z.filesystem)
	// defer z.state.unlockFilesystem(z.filesystem)

	// TODO implement a sort of proxy layer so that pushes can make it to the
	// right master, even if they land on the wrong server.

	state, err := z.state.getCurrentState(z.filesystem)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(fmt.Sprintf("Can't find state of filesystem %s.\n", z.filesystem)))
		return
	}
	master := z.state.masterFor(z.filesystem)
	if master == z.state.myNodeId {
		if state != "pushPeerState" {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(fmt.Sprintf(
				"Host is master for this filesystem (%s), but can't write to it "+
					"because state is %s.\n", z.filesystem, state)))
			return
		}
		// else OK, we can proceed
	} else {
		addresses := z.state.addressesFor(master)
		// XXX hack, IPv4 happens to come before IPv6 and happens to be routeable
		// on my network (whereas IPv6 isn't), but this depends on the enumeration
		// order of network cards in servers :/
		// TODO we should really attempt each address in turn until we find one
		// that works.
		peerAddress := addresses[0]

		url := fmt.Sprintf(
			"%s/filesystems/%s/%s/%s",
			deduceUrl(peerAddress, "internal"), // FIXME, need master->name mapping, see how handover works normally
			z.filesystem,
			z.fromSnap,
			z.toSnap,
		)

		// Proxy request to the master
		req, err := http.NewRequest(
			"POST", url,
			r.Body,
		)

		_, _, apiKey, err := getPasswords("admin")
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(fmt.Sprintf("Can't establish API key to proxy push: %+v.\n", err)))
			return
		}

		req.SetBasicAuth(
			"admin",
			apiKey,
		)
		postClient := new(http.Client)
		log.Printf("[ZFSReceiver] Proxying push to %s: %s", master, url)
		resp, err := postClient.Do(req)
		finished := make(chan bool)
		log.Printf("[ZFSReceiver] Got HTTP response +v", resp.StatusCode)
		w.WriteHeader(resp.StatusCode)
		go pipe(resp.Body, url,
			w, "proxied push recipient",
			finished,
			make(chan *Event),
			func(e *Event, c chan *Event) {},
			func(bytes int64, t int64) {},
			"compress",
		)
		defer resp.Body.Close()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(fmt.Sprintf("Can't proxy push to %s: %+v.\n", url, err)))
			return
		}
		log.Printf("[ZFSReceiver] Waiting for finish signal...")
		_ = <-finished
		return
	}

	cmd := exec.Command("zfs", "recv", fq(z.filesystem))
	pipeReader, pipeWriter := io.Pipe()
	defer pipeReader.Close()
	defer pipeWriter.Close()
	cmd.Stdin = pipeReader
	cmd.Stdout = w

	errBuffer := bytes.Buffer{}

	cmd.Stderr = &errBuffer
	finished := make(chan bool)

	go pipe(
		r.Body, fmt.Sprintf("http request body for %s", z.filesystem),
		pipeWriter, "zfs recv stdin", finished,
		make(chan *Event),
		func(e *Event, c chan *Event) {},
		func(bytes int64, t int64) {
			go func() {
				// ~~~~~~~~~~~~~
				// ~ THE BYTES ~
				// ~ THEY FLOW ~
				// ~ WORRY NOT ~
				// ~~~~~~~~~~~~~
				err := z.state.localReceiveProgress.Publish(z.filesystem, bytes)
				if err != nil {
					log.Printf("[ZFSReceiver] error notifying localReceiveProcess")
				}
			}()
		},
		"decompress",
	)

	log.Printf("[ZFSReceiver] about to start consuming prelude on %v", pipeReader)
	prelude, err := consumePrelude(pipeReader)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(fmt.Sprintf("Unable to parse prelude for %s: %s\n", z.filesystem, err)))
		return
	}
	log.Printf("[ZFSReceiver] Got prelude %v", prelude)

	err = cmd.Run()
	if err != nil {
		log.Printf(
			"Got error %s when running zfs recv for %s, check zfs-recv-stderr.log",
			err, z.filesystem,
		)
		pipeReader.Close()
		pipeWriter.Close()
		_ = <-finished
		readErr, err := ioutil.ReadAll(&errBuffer)
		if err != nil {
			// an error with your error. this is a bad day.
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(fmt.Sprintf("Unable to read error: %s\n", err)))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(fmt.Sprintf(
			"Unable to receive %s: %s, stderr: %s\n",
			z.filesystem, err, readErr,
		)))
		return
	}

	pipeReader.Close()
	pipeWriter.Close()
	_ = <-finished

	err = applyPrelude(prelude, fq(z.filesystem))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf("Unable to apply prelude for %s: %s\n", z.filesystem, err)))
		return
	}

	// XXX might this leak goroutines in any cases where fsMachine isn't in
	// pushPeerState when a push completes for some reason?
	go z.state.notifyNewSnapshotsAfterPush(z.filesystem)
	log.Printf("Closing pipe, and returning from ServeHTTP.")
}

type ZFSSender struct {
	state      *InMemoryState
	filesystem string
	fromSnap   string // "START" for "from the start"
	toSnap     string
}

type ZFSReceiver struct {
	state      *InMemoryState
	filesystem string
	fromSnap   string // "START" for "from the start"
	toSnap     string
}

func (s *InMemoryState) NewZFSSendingServer() http.Handler {
	return ZFSSender{
		state: s,
	}
}

func (s *InMemoryState) NewZFSReceivingServer() http.Handler {
	return ZFSReceiver{
		state: s,
	}
}
