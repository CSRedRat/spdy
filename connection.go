package spdy

import (
	"bufio"
  "crypto/tls"
	"fmt"
	"log"
	"net/http"
	"runtime"
  "sync"
	"time"
)

type connection struct {
	sync.Mutex
  remoteAddr         string // network address of remote side
	server             *http.Server
  conn               *tls.Conn
	buf                *bufio.Reader
  tlsState           *tls.ConnectionState
	tlsConfig          *tls.Config
  streams            map[uint32]*stream
  buffer             []Frame
  queue              []Frame
  nextServerStreamID uint32 // even
  nextClientStreamID uint32 // odd
  goaway             bool
	version            int
}

func (conn *connection) readFrames() {
	if d := conn.server.ReadTimeout; d != 0 {
		conn.conn.SetReadDeadline(time.Now().Add(d))
	}
	if d := conn.server.WriteTimeout; d != 0 {
		defer func() {
			conn.conn.SetWriteDeadline(time.Now().Add(d))
		}()
	}
	
	for {
		frame, err := ReadFrame(conn.buf)
		if err != nil {
			// TODO: handle error
			panic(err)
		}
		
		FrameHandling:
		switch frame := frame.(type) {
		default:
			panic(fmt.Sprintf("unexpected frame type %T", t))
			
			
	 /******************
	  *** SYN_STREAM ***
	  ******************/
		case SynStreamFrame:
			
			// Check stream creation is allowed.
			if conn.goaway {
				break FrameHandling
			}
			
			// Check version.
			if frame.Version != conn.version {
				log.Printf("Warning: Received frame with SPDY version %d on connection with version %d.\n",
					frame.Version, conn.version)
				if frame.Version > SPDY_VERSION {
					log.Printf("Error: Received frame with SPDY version %d, which is not supported.\n",
						frame.Version)
					reply := new(RstStreamFrame)
					reply.Version = SPDY_VERSION
					reply.StreamID = frame.StreamID
					reply.StatusCode = RST_STREAM_UNSUPPORTED_VERSION
					conn.WriteFrame(reply)
					break FrameHandling
				}
			}
			
			// Check Stream ID is odd.
			if frame.StreamID & 1 == 0 {
				log.Printf("Error: Received SYN_STREAM with Stream ID %d, which should be odd.\n",
					frame.StreamID)
				reply := new(RstStreamFrame)
				reply.Version = SPDY_VERSION
				reply.StreamID = frame.StreamID
				reply.StatusCode = RST_STREAM_PROTOCOL_ERROR
				conn.WriteFrame(reply)
				break FrameHandling
			}
			
			// Check Stream ID is the right number.
			if frame.StreamID != conn.nextClientStreamID + 2 {
				log.Printf("Error: Received SYN_STREAM with Stream ID %d, which should be %d.\n",
					frame.StreamID, conn.nextClientStreamID + 2)
				reply := new(RstStreamFrame)
				reply.Version = SPDY_VERSION
				reply.StreamID = frame.StreamID
				reply.StatusCode = RST_STREAM_PROTOCOL_ERROR
				conn.WriteFrame(reply)
				break FrameHandling
			}
			
			// Check Stream ID is not too large.
			if frame.StreamID > MAX_STREAM_ID {
				log.Printf("Error: Received SYN_STREAM with Stream ID %d, which is too large.\n",
					frame.StreamID)
				reply := new(RstStreamFrame)
				reply.Version = SPDY_VERSION
				reply.StreamID = frame.StreamID
				reply.StatusCode = RST_STREAM_PROTOCOL_ERROR
				conn.WriteFrame(reply)
				break FrameHandling
			}
			
			// Stream ID is fine.
			
			// Create and start new stream.
			conn.streams[frame.StreamID] = newStream(frame)
			go conn.streams[frame.StreamID].run()
			
		case SynReplyFrame:
			//
			
		case RstStreamFrame:
			//
			
		case SettingsFrame:
			//
			
		case PingFrame:
			//
			
		case GoawayFrame:
			//
			
		case HeadersFrame:
			//
			
		case WindowUpdateFrame:
			//
			
		case CredentialFrame:
			//
			
		case DataFrame:
			//
		}
	}
}

func (conn *connection) serve() {
	defer func() {
		if err := recover(); err != nil {
			const size = 4096
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			log.Printf("spdy: panic serving %v: %v\n%s", conn.remoteAddr, err, buf)
		}
	}()
	
	conn.readRequests()
}

func acceptSPDYVersion2(server *http.Server, tlsConn *tls.Conn, _ http.Handler) {
	conn := newConn(tlsConn)
	conn.server = server
	conn.tlsConfig = server.TLSConfig
	conn.version = 2
	
	conn.serve()
}

func acceptSPDYVersion3(server *http.Server, tlsConn *tls.Conn, _ http.Handler) {
	conn := newConn(tlsConn)
	conn.server = server
	conn.tlsConfig = server.TLSConfig
	conn.version = 3
	
	conn.serve()
}

func newConn(tlsConn *tls.Conn) *connection {
	conn := new(connection)
	conn.remoteAddr = tlsConn.RemoteAddr().String()
	conn.conn = tlsConn
	conn.buf = bufio.NewReader(tlsConn)
	*conn.tlsState = tlsConn.ConnectionState()
	conn.streams = make(map[uint32]*stream)
	conn.buffer = make([]Frame, 0, 10)
	conn.queue = make([]Frame, 0, 10)
	conn.nextServerStreamID = 0
	conn.nextClientStreamID = 1 - 2
	
	return conn
}
