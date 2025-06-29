package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

const VERSION = "1.0.0"

//=============================================================================
// COMMAND LINE ARGUMENT PARSING
//=============================================================================

type Arguments struct {
	SrtlaPort   int
	SrtHostname string
	SrtPort     int
	Verbose     bool
}

// Global variable to hold parsed arguments
var args Arguments

func parseArgs() {
	// Define flags
	flag.IntVar(&args.SrtlaPort, "srtla_port", 5000, "Port to bind the SRTLA socket to")
	flag.StringVar(&args.SrtHostname, "srt_hostname", "127.0.0.1", "Hostname of the downstream SRT server")
	flag.IntVar(&args.SrtPort, "srt_port", 5001, "Port of the downstream SRT server")
	flag.BoolVar(&args.Verbose, "verbose", false, "Enable verbose logging")

	versionFlag := flag.Bool("version", false, "Show version information")

	// Custom usage message
	flag.Usage = showHelp

	// Parse the flags
	flag.Parse()

	if *versionFlag {
		fmt.Println(VERSION)
		os.Exit(0)
	}
}

func showHelp() {
	fmt.Printf("srtla_rec v%s\n\n", VERSION)
	fmt.Println("Usage: go run main.go [options]")
	fmt.Println("\nOptions:")
	flag.PrintDefaults()
}

//=============================================================================
// LOGGING
//=============================================================================

func logInfo(format string, v ...interface{}) {
	log.Printf("[INFO] "+format, v...)
}

func logError(format string, v ...interface{}) {
	log.Printf("[ERROR] "+format, v...)
}

func logWarn(format string, v ...interface{}) {
	log.Printf("[WARN] "+format, v...)
}

func logDebug(format string, v ...interface{}) {
	if args.Verbose {
		log.Printf("[DEBUG] "+format, v...)
	}
}

func logCritical(format string, v ...interface{}) {
	log.Fatalf("[CRITICAL] "+format, v...)
}

// =============================================================================
// CONSTANTS
// =============================================================================
const (
	SRT_TYPE_ACK         = 0x8002
	SRTLA_TYPE_KEEPALIVE = 0x9000
	SRTLA_TYPE_ACK       = 0x9100
	SRTLA_TYPE_REG1      = 0x9200
	SRTLA_TYPE_REG2      = 0x9201
	SRTLA_TYPE_REG3      = 0x9202
	SRTLA_TYPE_REG_ERR   = 0x9210
	SRTLA_TYPE_REG_NGP   = 0x9211
	SRT_MIN_LEN          = 16
	SRTLA_ID_LEN         = 256
	SRTLA_TYPE_REG1_LEN  = 2 + SRTLA_ID_LEN
	SRTLA_TYPE_REG2_LEN  = 2 + SRTLA_ID_LEN
	SRTLA_TYPE_REG3_LEN  = 2
	SEND_BUF_SIZE        = 32 * 1024 * 1024
	RECV_BUF_SIZE        = 32 * 1024 * 1024
	MTU_SIZE             = 1500
	MAX_CONNS_PER_GROUP  = 16
	MAX_GROUPS           = 200
	CLEANUP_PERIOD_S     = 3 * time.Second
	GROUP_TIMEOUT_S      = 10 * time.Second
	CONN_TIMEOUT_S       = 10 * time.Second
	RECV_ACK_INT         = 10
)

//=============================================================================
// DATA STRUCTURES
//============================================================================

// SrtlaConn represents a single client connection.
type SrtlaConn struct {
	addr     *net.UDPAddr
	lastRcvd time.Time
	recvIdx  int
	recvLog  []uint32
	addrKey  string
}

// SrtlaConnGroup represents a logical group of connections.
type SrtlaConnGroup struct {
	sync.RWMutex
	id          []byte
	idKey       string
	connsByAddr map[string]*SrtlaConn
	createdAt   time.Time
	srtSocket   *net.UDPConn
	lastAddr    *net.UDPAddr
}

func newSrtlaConnGroup(clientID []byte, ts time.Time) *SrtlaConnGroup {
	id := make([]byte, SRTLA_ID_LEN)
	copy(id, clientID)
	// Fill the second half with random bytes
	_, err := rand.Read(id[SRTLA_ID_LEN/2:])
	if err != nil {
		// This is a critical failure, as we can't generate secure IDs.
		logCritical("Failed to generate random bytes for group ID: %v", err)
	}

	return &SrtlaConnGroup{
		id:          id,
		idKey:       hex.EncodeToString(id),
		connsByAddr: make(map[string]*SrtlaConn),
		createdAt:   ts,
	}
}

func (g *SrtlaConnGroup) getShortId() string {
	return g.idKey[:16] // 8 bytes -> 16 hex chars
}

func (g *SrtlaConnGroup) destroy() {
	g.Lock()
	defer g.Unlock()
	if g.srtSocket != nil {
		g.srtSocket.Close()
		g.srtSocket = nil
	}
	logInfo("[Group: %s] Group destroyed.", g.getShortId())
}

//=============================================================================
// GLOBAL STATE
//=============================================================================

// srtlaSocket is the main listening socket for client connections.
var srtlaSocket *net.UDPConn

// srtAddress is the resolved address of the downstream SRT server.
var srtAddress *net.UDPAddr

// groupsById tracks groups by their unique hex ID.
var groupsById = make(map[string]*SrtlaConnGroup)

// groupsByAddr tracks which group a client address belongs to.
var groupsByAddr = make(map[string]*SrtlaConnGroup)

// globalMutex protects access to the global maps.
var globalMutex = &sync.RWMutex{}

//=============================================================================
// UTILITIES
//=============================================================================

func getAddrKey(addr *net.UDPAddr) string {
	return addr.String()
}

func getSrtType(pkt []byte) uint16 {
	if len(pkt) < 2 {
		return 0
	}
	return binary.BigEndian.Uint16(pkt[0:2])
}

func getSrtSn(pkt []byte) int32 {
	if len(pkt) < 4 {
		return -1
	}
	sn := binary.BigEndian.Uint32(pkt[0:4])
	if (sn & 0x80000000) == 0 {
		return int32(sn)
	}
	return -1
}

func isSrtAck(pkt []byte) bool {
	return getSrtType(pkt) == SRT_TYPE_ACK
}

func isSrtlaKeepalive(pkt []byte) bool {
	return getSrtType(pkt) == SRTLA_TYPE_KEEPALIVE
}

func isSrtlaReg1(pkt []byte) bool {
	return len(pkt) == SRTLA_TYPE_REG1_LEN && getSrtType(pkt) == SRTLA_TYPE_REG1
}

func isSrtlaReg2(pkt []byte) bool {
	return len(pkt) == SRTLA_TYPE_REG2_LEN && getSrtType(pkt) == SRTLA_TYPE_REG2
}

func isSrtlaReg3(pkt []byte) bool {
	return len(pkt) == SRTLA_TYPE_REG3_LEN && getSrtType(pkt) == SRTLA_TYPE_REG3
}

//=============================================================================
// CORE LOGIC
//=============================================================================

func removeGroup(group *SrtlaConnGroup) {
	// Assumes globalMutex write lock is held
	delete(groupsById, group.idKey)
	group.RLock()
	for _, conn := range group.connsByAddr {
		delete(groupsByAddr, conn.addrKey)
	}
	if group.lastAddr != nil {
		delete(groupsByAddr, getAddrKey(group.lastAddr))
	}
	group.RUnlock()
	group.destroy()
}

func sendTo(addr *net.UDPAddr, packet []byte) {
	_, err := srtlaSocket.WriteToUDP(packet, addr)
	if err != nil {
		logError("Failed to send packet to %s: %v", getAddrKey(addr), err)
	}
}

func registerGroup(addr *net.UDPAddr, inBuf []byte, ts time.Time) {
	addrKey := getAddrKey(addr)

	globalMutex.Lock()
	defer globalMutex.Unlock()

	if len(groupsById) >= MAX_GROUPS {
		sendTo(addr, []byte{SRTLA_TYPE_REG_ERR >> 8, SRTLA_TYPE_REG_ERR & 0xff})
		logError("[%s] Group registration failed: Max groups reached", addrKey)
		return
	}

	if _, ok := groupsByAddr[addrKey]; ok {
		sendTo(addr, []byte{SRTLA_TYPE_REG_ERR >> 8, SRTLA_TYPE_REG_ERR & 0xff})
		logError("[%s] Group registration failed: Remote address already registered", addrKey)
		return
	}

	clientID := inBuf[2:]
	group := newSrtlaConnGroup(clientID, ts)
	group.lastAddr = addr

	outBuf := make([]byte, SRTLA_TYPE_REG2_LEN)
	binary.BigEndian.PutUint16(outBuf[0:2], SRTLA_TYPE_REG2)
	copy(outBuf[2:], group.id)

	sendTo(addr, outBuf)

	groupsById[group.idKey] = group
	groupsByAddr[addrKey] = group
	logInfo("[%s] [Group: %s] Group registered", addrKey, group.getShortId())
}

func connReg(addr *net.UDPAddr, inBuf []byte, ts time.Time) {
	addrKey := getAddrKey(addr)
	id := hex.EncodeToString(inBuf[2:])

	globalMutex.Lock()
	defer globalMutex.Unlock()

	group, ok := groupsById[id]
	if !ok {
		sendTo(addr, []byte{SRTLA_TYPE_REG_NGP >> 8, SRTLA_TYPE_REG_NGP & 0xff})
		logError("[%s] Connection registration failed: No group found for id %s", addrKey, id)
		return
	}

	group.Lock()
	defer group.Unlock()

	existingGroupForAddr, ok := groupsByAddr[addrKey]
	if ok && existingGroupForAddr != group {
		sendTo(addr, []byte{SRTLA_TYPE_REG_ERR >> 8, SRTLA_TYPE_REG_ERR & 0xff})
		logError("[%s] [Group: %s] Connection registration failed: Provided group ID mismatch", addrKey, group.getShortId())
		return
	}

	alreadyRegistered := false
	if _, ok := group.connsByAddr[addrKey]; ok {
		alreadyRegistered = true
	}

	if !alreadyRegistered && len(group.connsByAddr) >= MAX_CONNS_PER_GROUP {
		sendTo(addr, []byte{SRTLA_TYPE_REG_ERR >> 8, SRTLA_TYPE_REG_ERR & 0xff})
		logError("[%s] [Group: %s] Connection registration failed: Max group conns reached", addrKey, group.getShortId())
		return
	}

	sendTo(addr, []byte{SRTLA_TYPE_REG3 >> 8, SRTLA_TYPE_REG3 & 0xff})

	if !alreadyRegistered {
		newConn := &SrtlaConn{
			addr:     addr,
			lastRcvd: ts,
			recvLog:  make([]uint32, RECV_ACK_INT),
			addrKey:  addrKey,
		}
		group.connsByAddr[addrKey] = newConn
		groupsByAddr[addrKey] = group
	}

	group.lastAddr = addr
	logInfo("[%s] [Group: %s] Connection registration successful", addrKey, group.getShortId())
}

func registerPacket(conn *SrtlaConn, sn int32) {
	conn.recvLog[conn.recvIdx] = uint32(sn)
	conn.recvIdx++
	if conn.recvIdx == RECV_ACK_INT {
		ackPacket := make([]byte, 4+4*RECV_ACK_INT)
		headerValue := uint32(SRTLA_TYPE_ACK << 16)
		binary.BigEndian.PutUint32(ackPacket[0:4], headerValue)
		for i := 0; i < RECV_ACK_INT; i++ {
			binary.BigEndian.PutUint32(ackPacket[4+i*4:], conn.recvLog[i])
		}
		sendTo(conn.addr, ackPacket)
		conn.recvIdx = 0
	}
}

func handleSrtData(group *SrtlaConnGroup, msg []byte) {
	if len(msg) < SRT_MIN_LEN {
		logError("[Group: %s] Invalid SRT packet received from server, length %d. Terminating group.", group.getShortId(), len(msg))
		globalMutex.Lock()
		removeGroup(group)
		globalMutex.Unlock()
		return
	}

	group.RLock()
	defer group.RUnlock()

	if isSrtAck(msg) {
		for _, conn := range group.connsByAddr {
			sendTo(conn.addr, msg)
		}
	} else if group.lastAddr != nil {
		sendTo(group.lastAddr, msg)
	}
}

func handleSrtlaData(msg []byte, rinfo *net.UDPAddr) {
	ts := time.Now()

	if isSrtlaReg1(msg) {
		registerGroup(rinfo, msg, ts)
		return
	}
	if isSrtlaReg2(msg) {
		connReg(rinfo, msg, ts)
		return
	}

	addrKey := getAddrKey(rinfo)

	globalMutex.RLock()
	group, ok := groupsByAddr[addrKey]
	globalMutex.RUnlock()

	if !ok {
		logDebug("Discarding packet from unknown source %s", addrKey)
		return
	}

	group.Lock() // Lock group for modification

	conn, ok := group.connsByAddr[addrKey]
	if !ok {
		group.Unlock()
		logDebug("Discarding packet from known group but unknown conn %s", addrKey)
		return
	}

	conn.lastRcvd = ts
	group.lastAddr = rinfo

	if isSrtlaKeepalive(msg) {
		sendTo(rinfo, msg)
		group.Unlock()
		return
	}

	if len(msg) < SRT_MIN_LEN {
		logDebug("[Group: %s] Discarding short packet (%d bytes)", group.getShortId(), len(msg))
		group.Unlock()
		return
	}

	sn := getSrtSn(msg)
	if sn >= 0 {
		registerPacket(conn, sn)
	}

	forwardPacket := func() bool {
		if group.srtSocket == nil {
			group.Unlock() // must unlock before removeGroup to avoid deadlock
			globalMutex.Lock()
			removeGroup(group)
			globalMutex.Unlock()
			return false // indicate that group was unlocked
		}
		_, err := group.srtSocket.Write(msg)
		if err != nil {
			logError("[Group: %s] Failed to forward packet: %v. Terminating group.", group.getShortId(), err)
			group.Unlock() // must unlock before removeGroup to avoid deadlock
			globalMutex.Lock()
			removeGroup(group)
			globalMutex.Unlock()
			return false // indicate that group was unlocked
		}
		return true // indicate that group is still locked
	}

	if group.srtSocket == nil {
		sock, err := net.DialUDP("udp", nil, srtAddress)
		if err != nil {
			logError("[Group: %s] Failed to create downstream SRT socket: %v", group.getShortId(), err)
			group.Unlock()
			return
		}

		err = sock.SetReadBuffer(RECV_BUF_SIZE)
		if err != nil {
			logWarn("Could not set read buffer size on downstream socket: %v", err)
		}
		err = sock.SetWriteBuffer(SEND_BUF_SIZE)
		if err != nil {
			logWarn("Could not set write buffer size on downstream socket: %v", err)
		}

		group.srtSocket = sock
		logInfo("[Group: %s] Created SRT socket connected to %s. Local Port: %s", group.getShortId(), srtAddress.String(), sock.LocalAddr().String())

		// Start a goroutine to listen for messages from the downstream server
		go func(g *SrtlaConnGroup) {
			buf := make([]byte, MTU_SIZE)
			for {
				n, err := g.srtSocket.Read(buf)
				if err != nil {
					// Socket closed, goroutine can exit
					g.RLock()
					isSocketNil := g.srtSocket == nil
					g.RUnlock()
					if !isSocketNil {
						logInfo("[Group: %s] Downstream socket read error, likely closed: %v", g.getShortId(), err)
					}
					return
				}
				handleSrtData(g, buf[:n])
			}
		}(group)
	}

	if forwardPacket() {
		group.Unlock() // Everything done, unlock group only if still locked
	}
}

func cleanupGroupsAndConnections() {
	logDebug("Starting cleanup run...")
	ts := time.Now()
	groupTimeout := GROUP_TIMEOUT_S
	connTimeout := CONN_TIMEOUT_S

	removedGroups := 0
	removedConns := 0

	globalMutex.Lock()
	defer globalMutex.Unlock()

	for _, group := range groupsById {
		group.Lock()
		initialConnCount := len(group.connsByAddr)

		for key, conn := range group.connsByAddr {
			if ts.Sub(conn.lastRcvd) > connTimeout {
				logInfo("[%s] [Group: %s] Connection removed (timed out)", conn.addrKey, group.getShortId())
				delete(group.connsByAddr, key)
				delete(groupsByAddr, key)
				removedConns++
			}
		}

		if len(group.connsByAddr) == 0 && initialConnCount > 0 && ts.Sub(group.createdAt) > groupTimeout {
			logInfo("[Group: %s] Group removed (no connections and timed out)", group.getShortId())
			// removeGroup expects global lock to be held, which it is
			// It also calls group.destroy, which takes a group lock, but we are holding it.
			// To avoid deadlock, we must release the group lock before calling removeGroup.
			// However, removeGroup also needs to modify groupsByAddr, which might be in use
			// by the group. A copy is safer.
			connsToRemove := make([]*SrtlaConn, 0, len(group.connsByAddr))
			for _, c := range group.connsByAddr {
				connsToRemove = append(connsToRemove, c)
			}
			lastAddrKey := ""
			if group.lastAddr != nil {
				lastAddrKey = getAddrKey(group.lastAddr)
			}

			group.Unlock() // Release group lock

			delete(groupsById, group.idKey)
			for _, conn := range connsToRemove {
				delete(groupsByAddr, conn.addrKey)
			}
			if lastAddrKey != "" {
				delete(groupsByAddr, lastAddrKey)
			}
			group.destroy()
			removedGroups++
			continue // skip to next group in outer loop
		}
		group.Unlock()
	}
	logDebug("Cleanup run ended. Removed %d groups and %d connections.", removedGroups, removedConns)
}

//=============================================================================
// MAIN EXECUTION
//=============================================================================

func main() {
	parseArgs()

	logInfo("srtla_rec v%s starting...", VERSION)
	logInfo("Config - SRTLA Port: %d, SRT Target: %s:%d, Verbose: %v",
		args.SrtlaPort, args.SrtHostname, args.SrtPort, args.Verbose)

	// Resolve downstream SRT server address
	var err error
	srtAddress, err = net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", args.SrtHostname, args.SrtPort))
	if err != nil {
		logCritical("Failed to resolve downstream SRT hostname '%s': %v", args.SrtHostname, err)
	}
	logInfo("Downstream SRT server target set to %s", srtAddress.String())

	// Create main listening socket
	listenAddr := &net.UDPAddr{Port: args.SrtlaPort, IP: net.ParseIP("::")}
	srtlaSocket, err = net.ListenUDP("udp", listenAddr)
	if err != nil {
		logCritical("Failed to bind SRTLA listener on port %d: %v", args.SrtlaPort, err)
	}
	defer srtlaSocket.Close()

	err = srtlaSocket.SetReadBuffer(RECV_BUF_SIZE)
	if err != nil {
		logWarn("Could not set read buffer size on main socket: %v", err)
	}
	err = srtlaSocket.SetWriteBuffer(SEND_BUF_SIZE)
	if err != nil {
		logWarn("Could not set write buffer size on main socket: %v", err)
	}

	logInfo("srtla_rec is now running, listening on %s", srtlaSocket.LocalAddr().String())

	// Set up context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start periodic tasks
	go func() {
		ticker := time.NewTicker(CLEANUP_PERIOD_S)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				cleanupGroupsAndConnections()
			case <-ctx.Done():
				return
			}
		}
	}()

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		logInfo("Shutting down...")

		globalMutex.RLock()
		for _, group := range groupsById {
			group.destroy()
		}
		globalMutex.RUnlock()

		cancel()
		srtlaSocket.Close() // This will cause the ReadFromUDP loop to exit
	}()

	// Main listening loop
	buffer := make([]byte, MTU_SIZE)
	for {
		n, rinfo, err := srtlaSocket.ReadFromUDP(buffer)
		if err != nil {
			select {
			case <-ctx.Done(): // Expected error on shutdown
				logInfo("Listener stopped.")
				return
			default: // Unexpected error
				logError("SRTLA socket read error: %v", err)
			}
			return
		}

		// Make a copy of the slice to handle it concurrently
		msg := make([]byte, n)
		copy(msg, buffer[:n])

		// Handle packet in a new goroutine to avoid blocking the listener
		go handleSrtlaData(msg, rinfo)
	}
}
