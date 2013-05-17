package spdy

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"runtime"
	"sync"
	"time"
)

// clientConnection represents a SPDY session at the client
// end. This performs the overall connection management and
// co-ordination between streams.
type clientConnection struct {
	sync.RWMutex
	remoteAddr         string
	client             *Client
	conn               *tls.Conn
	buf                *bufio.Reader // buffered reader for the connection.
	tlsState           *tls.ConnectionState
	streams            map[uint32]Stream
	streamInputs       map[uint32]chan<- Frame
	dataOutput         chan Frame
	pings              map[uint32]chan<- bool
	pingID             uint32
	compressor         *Compressor
	decompressor       *Decompressor
	receivedSettings   map[uint32]*Setting
	nextServerStreamID uint32          // even
	nextClientStreamID uint32          // odd
	initialWindowSize  uint32          // transport window
	goaway             bool            // goaway has been sent/received.
	version            int             // SPDY version.
	numBenignErrors    int             // number of non-serious errors encountered.
	done               *sync.WaitGroup // WaitGroup for active streams.
}

// readFrames is the main processing loop, where frames
// are read from the connection and processed individually.
// Returning from readFrames begins the cleanup and exit
// process for this connection.
func (conn *clientConnection) readFrames() {

	// Add timeouts if requested by the server.
	if d := conn.client.ReadTimeout; d != 0 {
		conn.conn.SetReadDeadline(time.Now().Add(d))
	}
	if d := conn.client.WriteTimeout; d != 0 {
		defer func() {
			conn.conn.SetWriteDeadline(time.Now().Add(d))
		}()
	}

	// Main loop.
	for {

		// This is the mechanism for handling too many benign errors.
		// Default MaxBenignErrors is 10.
		if conn.numBenignErrors > MaxBenignErrors {
			log.Println("Error: Too many invalid stream IDs received. Ending connection.")
			conn.PROTOCOL_ERROR(0)
		}

		// ReadFrame takes care of the frame parsing for us.
		frame, err := ReadFrame(conn.buf)
		if err != nil {
			if err == io.EOF {
				// Client has closed the TCP connection.
				return
			}

			// TODO: handle error
			panic(err)
		}

		// Decompress the frame's headers, if there are any.
		err = frame.ReadHeaders(conn.decompressor)
		if err != nil {
			panic(err)
		}

		// TODO: replace this with a proper logging library.
		if DebugMode {
			fmt.Println("Received Frame:")
			fmt.Println(frame)
		}

		// Make sure the received frame uses an appropriate
		// SPDY version.
		if !conn.validFrameVersion(frame) {
			reply := new(RstStreamFrame)
			reply.version = SPDY_VERSION
			reply.streamID = frame.StreamID()
			reply.StatusCode = RST_STREAM_UNSUPPORTED_VERSION
			conn.WriteFrame(reply)
			continue
		}

	FrameHandling:
		// This is the main frame handling section.
		switch frame := frame.(type) {

		/*** [UNIMPLEMENTED] ***/
		case *SynStreamFrame:
			log.Println("Got SYN_STREAM: [UNIMPLEMENTED]")

		/*** [UNIMPLEMENTED] ***/
		case *SynReplyFrame:
			log.Println("Got SYN_REPLY: [UNIMPLEMENTED]")

		case *RstStreamFrame:
			if StatusCodeIsFatal(int(frame.StatusCode)) {
				code := StatusCodeText(int(frame.StatusCode))
				log.Printf("Warning: Received %s on stream %d. Closing stream.\n", code, frame.StreamID)
				return
			}
			conn.handleRstStream(frame)

		case *SettingsFrame:
			for _, setting := range frame.Settings {
				conn.receivedSettings[setting.ID] = setting
				if setting.ID == SETTINGS_INITIAL_WINDOW_SIZE && conn.version > 2 {
					log.Printf("Initial window size is %d.\n", setting.Value)
					conn.initialWindowSize = setting.Value
				}
			}

		case *PingFrame:
			// Check whether Ping ID is client-sent.
			if frame.PingID&1 != 0 {
				if conn.pings[frame.PingID] == nil {
					log.Printf("Warning: Ignored PING with Ping ID %d, which hasn't been requested.\n",
						frame.PingID)
					conn.numBenignErrors++
					break FrameHandling
				}
				conn.pings[frame.PingID] <- true
				close(conn.pings[frame.PingID])
				delete(conn.pings, frame.PingID)
			} else {
				// TODO: Print to the log in DebugMode only.
				log.Println("Received PING. Replying...")
				conn.WriteFrame(frame)
			}

		case *GoawayFrame:

			lastProcessed := frame.LastGoodStreamID
			for streamID, stream := range conn.streams {
				if streamID&1 != 0 && streamID > lastProcessed {
					// Stream is client-sent and has not been processed.
					stream.Cancel()
				}
			}
			conn.goaway = true

		case *HeadersFrame:
			conn.handleHeadersFrame(frame)

		case *WindowUpdateFrame:
			conn.handleWindowUpdateFrame(frame)

		/*** [UNIMPLEMENTED] ***/
		case *CredentialFrame:
			log.Println("Got CREDENTIAL: [UNIMPLEMENTED]")

		case *DataFrame:
			conn.handleDataFrame(frame)

		default:
			log.Println(fmt.Sprintf("unexpected frame type %T", frame))
		}
	}
}

// send is run in a separate goroutine. It's used
// to ensure clear interleaving of frames and to
// provide assurances of priority and structure.
func (conn *clientConnection) send() {
	for {
		frame := <-conn.dataOutput

		// Compress any name/value header blocks.
		err := frame.WriteHeaders(conn.compressor)
		if err != nil {
			panic(err)
		}

		// Leave the specifics of writing to the
		// connection up to the frame.
		err = frame.WriteTo(conn.conn)
		if err != nil {
			panic(err)
		}
	}
}

// Internally-sent frames have high priority.
func (conn *clientConnection) WriteFrame(frame Frame) {
	conn.dataOutput <- frame
}

// Ping is used to send a SPDY ping to the client.
// A channel is returned immediately, and 'true'
// sent when the ping reply is received. If there
// is a fault in the connection, the channel is
// closed.
func (conn *clientConnection) Ping() <-chan bool {
	conn.Lock()
	defer conn.Unlock()

	ping := new(PingFrame)
	ping.version = uint16(conn.version)
	ping.PingID = conn.pingID
	conn.dataOutput <- ping
	c := make(chan bool, 1)
	conn.pings[conn.pingID] = c
	conn.pingID += 2
	return c
}

// Push is a method stub required to satisfy the Connection
// interface. It must not be used by clients.
func (conn *clientConnection) Push(resource string, origin Stream) (PushWriter, error) {
	return nil, errors.New("Error: Clients cannot send pushes.")
}

func (conn *clientConnection) Request(req *Request) (Stream, error) {
	return nil, nil
}

// validFrameVersion checks that a frame has the same SPDY
// version number as the rest of the connection. This library
// does not support the mixing of different versions within a
// connection, even if the library supports all versions being
// used.
func (conn *clientConnection) validFrameVersion(frame Frame) bool {

	// DATA frames have no version, so they
	// are always valid.
	if _, ok := frame.(*DataFrame); ok {
		return true
	}

	// Check the version.
	if frame.Version() != uint16(conn.version) {
		log.Printf("Error: Received frame with SPDY version %d on connection with version %d.\n",
			frame.Version(), conn.version)
		if frame.Version() > SPDY_VERSION {
			log.Printf("Error: Received frame with SPDY version %d, which is not supported.\n",
				frame.Version())
		}
		return false
	}
	return true
}

// handleSynStream performs the processing of SYN_STREAM frames.
func (conn *clientConnection) handleSynStream(frame *SynStreamFrame) {
	conn.RLock()
	defer func() { conn.RUnlock() }()

	// Check stream creation is allowed.
	if conn.goaway {
		return
	}

	sid := frame.streamID

	// Check Stream ID is even.
	if sid&1 != 0 {
		log.Printf("Error: Received SYN_STREAM with Stream ID %d, which should be even.\n", sid)
		conn.numBenignErrors++
		return
	}

	// Check Stream ID is the right number.
	nsid := conn.nextServerStreamID + 2
	if sid != nsid {
		log.Printf("Error: Received SYN_STREAM with Stream ID %d, which should be %d.\n", sid, nsid)
		conn.numBenignErrors++
		return
	}

	// Check Stream ID is not out of bounds.
	if sid > MAX_STREAM_ID {
		log.Printf("Error: Received SYN_STREAM with Stream ID %d, which exceeds the limit.\n", sid)
		conn.PROTOCOL_ERROR(sid)
	}

	// Stream ID is fine.

	// Create and start new stream.
	conn.RUnlock()
	conn.Lock()

	// TODO: add handling here.

	conn.Unlock()
	conn.RLock()

	//go nextStream.run()
	conn.done.Add(1)

	return
}

// handleRstStream performs the processing of RST_STREAM frames.
func (conn *clientConnection) handleRstStream(frame *RstStreamFrame) {
	conn.RLock()
	defer func() { conn.RUnlock() }()

	sid := frame.streamID

	switch frame.StatusCode {
	case RST_STREAM_INVALID_STREAM:
		log.Printf("Error: Received INVALID_STREAM for stream ID %d.\n", sid)
		conn.numBenignErrors++
		return

	case RST_STREAM_REFUSED_STREAM:
		conn.closeStream(sid)
		return

	case RST_STREAM_CANCEL:
		if sid&1 == 0 {
			log.Printf("Error: Received RST_STREAM with Stream ID %d, which should be odd.\n", sid)
			conn.numBenignErrors++
			return
		}
		conn.closeStream(sid)
		return

	case RST_STREAM_FLOW_CONTROL_ERROR:
		log.Printf("Error: Received FLOW_CONTROL_ERROR for stream ID %d.\n", sid)
		conn.numBenignErrors++
		return

	case RST_STREAM_STREAM_IN_USE:
		log.Printf("Error: Received STREAM_IN_USE for stream ID %d.\n", sid)
		conn.numBenignErrors++
		return

	case RST_STREAM_STREAM_ALREADY_CLOSED:
		log.Printf("Error: Received STREAM_ALREADY_CLOSED for stream ID %d.\n", sid)
		conn.numBenignErrors++
		return

	case RST_STREAM_INVALID_CREDENTIALS:
		log.Printf("Error: Received INVALID_CREDENTIALS for stream ID %d.\n", sid)
		conn.numBenignErrors++
		return

	default:
		log.Printf("Error: Received unknown RST_STREAM status code %d.\n", frame.StatusCode)
		conn.PROTOCOL_ERROR(sid)
	}
}

// handleDataFrame performs the processing of DATA frames.
func (conn *clientConnection) handleDataFrame(frame *DataFrame) {
	conn.RLock()
	defer func() { conn.RUnlock() }()

	sid := frame.streamID

	// Check Stream ID is odd.
	if sid&1 == 0 {
		log.Printf("Error: Received DATA with Stream ID %d, which should be odd.\n", sid)
		conn.numBenignErrors++
		return
	}

	// Check stream is open.
	nsid := conn.nextClientStreamID + 2
	if sid != nsid && sid != 1 && conn.nextClientStreamID != 0 {
		log.Printf("Error: Received DATA with Stream ID %d, which should be %d.\n", sid, nsid)
		conn.numBenignErrors++
		return
	}

	// Stream ID is fine.

	// Send data to stream.
	conn.streamInputs[sid] <- frame

	// Handle flags.
	if frame.Flags&FLAG_FIN != 0 {
		conn.streams[sid].State().CloseThere()
	}
}

// handleHeadersFrame performs the processing of HEADERS frames.
func (conn *clientConnection) handleHeadersFrame(frame *HeadersFrame) {
	conn.RLock()
	defer func() { conn.RUnlock() }()

	sid := frame.streamID

	// Check Stream ID is odd.
	if sid&1 == 0 {
		log.Printf("Error: Received HEADERS with Stream ID %d, which should be odd.\n", sid)
		conn.numBenignErrors++
		return
	}

	// Check stream is open.
	nsid := conn.nextClientStreamID + 2
	if sid != nsid && sid != 1 && conn.nextClientStreamID != 0 {
		log.Printf("Error: Received HEADERS with Stream ID %d, which should be %d.\n", sid, nsid)
		conn.numBenignErrors++
		return
	}

	// Stream ID is fine.

	// Send headers to stream.
	conn.streamInputs[sid] <- frame

	// Handle flags.
	if frame.Flags&FLAG_FIN != 0 {
		conn.streams[sid].State().CloseThere()
	}
}

// handleWindowUpdateFrame performs the processing of WINDOW_UPDATE frames.
func (conn *clientConnection) handleWindowUpdateFrame(frame *WindowUpdateFrame) {
	conn.RLock()
	defer func() { conn.RUnlock() }()

	sid := frame.streamID

	// Check Stream ID is odd.
	if sid&1 == 0 {
		log.Printf("Error: Received WINDOW_UPDATE with Stream ID %d, which should be odd.\n", sid)
		conn.numBenignErrors++
		return
	}

	// Check stream is open.
	nsid := conn.nextClientStreamID + 2
	if sid != nsid && sid != 1 && conn.nextClientStreamID != 0 {
		log.Printf("Error: Received WINDOW_UPDATE with Stream ID %d, which should be %d.\n", sid, nsid)
		conn.numBenignErrors++
		return
	}

	// Stream ID is fine.

	// Check delta window size is valid.
	delta := frame.DeltaWindowSize
	if delta > MAX_DELTA_WINDOW_SIZE || delta < 1 {
		log.Printf("Error: Received WINDOW_UPDATE with invalid delta window size %d.\n", delta)
		conn.PROTOCOL_ERROR(sid)
	}

	// Send update to stream.
	conn.streamInputs[sid] <- frame
}

// closeStream closes the provided stream safely.
func (conn *clientConnection) closeStream(streamID uint32) {
	if streamID == 0 {
		log.Println("Error: Tried to close stream 0.")
		return
	}

	conn.streams[streamID].Stop()
	conn.streams[streamID].State().Close()
	close(conn.streamInputs[streamID])
	delete(conn.streams, streamID)
}

// PROTOCOL_ERROR informs the client that a protocol error has
// occurred, stops all running streams, and ends the connection.
func (conn *clientConnection) PROTOCOL_ERROR(streamID uint32) {
	reply := new(RstStreamFrame)
	reply.version = uint16(conn.version)
	reply.streamID = streamID
	reply.StatusCode = RST_STREAM_PROTOCOL_ERROR
	conn.WriteFrame(reply)

	// Leave time for the message to be sent and received.
	time.Sleep(100 * time.Millisecond)
	conn.cleanup()
	runtime.Goexit()
}

// cleanup is used to end any running streams and
// aid garbage collection before the connection
// is closed.
func (conn *clientConnection) cleanup() {
	for streamID, c := range conn.streamInputs {
		close(c)
		conn.streams[streamID].Stop()
	}
	conn.streamInputs = nil
	conn.streams = nil
}

// run prepares and executes the frame reading
// loop of the connection. At this point, any
// global settings set by the client are sent to
// the new client.
func (conn *clientConnection) run() {
	defer func() {
		if err := recover(); err != nil {
			const size = 4096
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			log.Printf("spdy: panic serving %v: %v\n%s", conn.remoteAddr, err, buf)
		}
	}()

	// Start the send loop.
	go conn.send()

	// Send any global settings.
	settings := new(SettingsFrame)
	settings.version = uint16(conn.version)
	settings.Settings = []*Setting{
		&Setting{
			ID:    SETTINGS_INITIAL_WINDOW_SIZE,
			Value: conn.initialWindowSize,
		},
		&Setting{
			ID:    SETTINGS_MAX_CONCURRENT_STREAMS,
			Value: 1000,
		},
	}
	if conn.client.GlobalSettings != nil {
		settings.Settings = append(settings.Settings, conn.client.GlobalSettings...)
	}
	conn.dataOutput <- settings

	// Enter the main loop.
	conn.readFrames()

	// Cleanup before the connection closes.
	conn.cleanup()
}

// newConn is used to create and initialise a server connection.
func newClientConn(tlsConn *tls.Conn) *clientConnection {
	conn := new(clientConnection)
	conn.remoteAddr = tlsConn.RemoteAddr().String()
	conn.conn = tlsConn
	conn.buf = bufio.NewReader(tlsConn)
	conn.tlsState = new(tls.ConnectionState)
	*conn.tlsState = tlsConn.ConnectionState()
	conn.compressor = new(Compressor)
	conn.decompressor = new(Decompressor)
	conn.initialWindowSize = DEFAULT_INITIAL_CLIENT_WINDOW_SIZE
	conn.streams = make(map[uint32]Stream)
	conn.streamInputs = make(map[uint32]chan<- Frame)
	conn.receivedSettings = make(map[uint32]*Setting)
	conn.dataOutput = make(chan Frame)
	conn.pings = make(map[uint32]chan<- bool)
	conn.pingID = 1
	conn.done = new(sync.WaitGroup)

	return conn
}
