package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"regexp"
	"time"

	"gopkg.in/alecthomas/kingpin.v2"
	"gopkg.in/yaml.v2"
	"gortc.io/sdp"
)

var Version = "v0.0.0"

func parseIpCidrList(in []string) ([]interface{}, error) {
	if len(in) == 0 {
		return nil, nil
	}

	var ret []interface{}
	for _, t := range in {
		_, ipnet, err := net.ParseCIDR(t)
		if err == nil {
			ret = append(ret, ipnet)
			continue
		}

		ip := net.ParseIP(t)
		if ip != nil {
			ret = append(ret, ip)
			continue
		}

		return nil, fmt.Errorf("unable to parse ip/network '%s'", t)
	}
	return ret, nil
}

type trackFlowType int

const (
	_TRACK_FLOW_RTP trackFlowType = iota
	_TRACK_FLOW_RTCP
)

type track struct {
	rtpPort  int
	rtcpPort int
}

type streamProtocol int

const (
	_STREAM_PROTOCOL_UDP streamProtocol = iota
	_STREAM_PROTOCOL_TCP
)

func (s streamProtocol) String() string {
	if s == _STREAM_PROTOCOL_UDP {
		return "udp"
	}
	return "tcp"
}

type programEvent interface {
	isProgramEvent()
}

type programEventClientNew struct {
	nconn net.Conn
}

func (programEventClientNew) isProgramEvent() {}

type programEventClientClose struct {
	done   chan struct{}
	client *serverClient
}

func (programEventClientClose) isProgramEvent() {}

type programEventClientDescribe struct {
	path string
	res  chan []byte
}

func (programEventClientDescribe) isProgramEvent() {}

type programEventClientAnnounce struct {
	res    chan error
	client *serverClient
	path   string
}

func (programEventClientAnnounce) isProgramEvent() {}

type programEventClientSetupPlay struct {
	res      chan error
	client   *serverClient
	path     string
	protocol streamProtocol
	rtpPort  int
	rtcpPort int
}

func (programEventClientSetupPlay) isProgramEvent() {}

type programEventClientSetupRecord struct {
	res      chan error
	client   *serverClient
	protocol streamProtocol
	rtpPort  int
	rtcpPort int
}

func (programEventClientSetupRecord) isProgramEvent() {}

type programEventClientPlay1 struct {
	res    chan error
	client *serverClient
}

func (programEventClientPlay1) isProgramEvent() {}

type programEventClientPlay2 struct {
	res    chan error
	client *serverClient
}

func (programEventClientPlay2) isProgramEvent() {}

type programEventClientPause struct {
	res    chan error
	client *serverClient
}

func (programEventClientPause) isProgramEvent() {}

type programEventClientRecord struct {
	res    chan error
	client *serverClient
}

func (programEventClientRecord) isProgramEvent() {}

type programEventClientFrameUdp struct {
	trackFlowType trackFlowType
	addr          *net.UDPAddr
	buf           []byte
}

func (programEventClientFrameUdp) isProgramEvent() {}

type programEventClientFrameTcp struct {
	path          string
	trackId       int
	trackFlowType trackFlowType
	buf           []byte
}

func (programEventClientFrameTcp) isProgramEvent() {}

type programEventStreamerReady struct {
	streamer *streamer
}

func (programEventStreamerReady) isProgramEvent() {}

type programEventStreamerNotReady struct {
	streamer *streamer
}

func (programEventStreamerNotReady) isProgramEvent() {}

type programEventStreamerFrame struct {
	streamer      *streamer
	trackId       int
	trackFlowType trackFlowType
	buf           []byte
}

func (programEventStreamerFrame) isProgramEvent() {}

type programEventTerminate struct{}

func (programEventTerminate) isProgramEvent() {}

type ConfPath struct {
	Source         string   `yaml:"source"`
	SourceProtocol string   `yaml:"sourceProtocol"`
	PublishUser    string   `yaml:"publishUser"`
	PublishPass    string   `yaml:"publishPass"`
	PublishIps     []string `yaml:"publishIps"`
	publishIps     []interface{}
	ReadUser       string   `yaml:"readUser"`
	ReadPass       string   `yaml:"readPass"`
	ReadIps        []string `yaml:"readIps"`
	readIps        []interface{}
}

type conf struct {
	Protocols    []string             `yaml:"protocols"`
	RtspPort     int                  `yaml:"rtspPort"`
	RtpPort      int                  `yaml:"rtpPort"`
	RtcpPort     int                  `yaml:"rtcpPort"`
	ReadTimeout  time.Duration        `yaml:"readTimeout"`
	WriteTimeout time.Duration        `yaml:"writeTimeout"`
	PreScript    string               `yaml:"preScript"`
	PostScript   string               `yaml:"postScript"`
	Pprof        bool                 `yaml:"pprof"`
	Paths        map[string]*ConfPath `yaml:"paths"`
}

func loadConf(fpath string, stdin io.Reader) (*conf, error) {
	if fpath == "stdin" {
		var ret conf
		err := yaml.NewDecoder(stdin).Decode(&ret)
		if err != nil {
			return nil, err
		}

		return &ret, nil

	} else {
		// conf.yml is optional
		if fpath == "conf.yml" {
			if _, err := os.Stat(fpath); err != nil {
				return &conf{}, nil
			}
		}

		f, err := os.Open(fpath)
		if err != nil {
			return nil, err
		}
		defer f.Close()

		var ret conf
		err = yaml.NewDecoder(f).Decode(&ret)
		if err != nil {
			return nil, err
		}

		return &ret, nil
	}
}

// a publisher can be either a serverClient or a streamer
type publisher interface {
	publisherIsReady() bool
	publisherSdpText() []byte
	publisherSdpParsed() *sdp.Message
}

type program struct {
	conf           *conf
	protocols      map[streamProtocol]struct{}
	tcpl           *serverTcpListener
	udplRtp        *serverUdpListener
	udplRtcp       *serverUdpListener
	clients        map[*serverClient]struct{}
	streamers      []*streamer
	publishers     map[string]publisher
	publisherCount int
	receiverCount  int

	events chan programEvent
	done   chan struct{}
}

func newProgram(sargs []string, stdin io.Reader) (*program, error) {
	k := kingpin.New("rtsp-simple-server",
		"rtsp-simple-server "+Version+"\n\nRTSP server.")

	argVersion := k.Flag("version", "print version").Bool()
	argConfPath := k.Arg("confpath", "path to a config file. The default is conf.yml. Use 'stdin' to read config from stdin").Default("conf.yml").String()

	kingpin.MustParse(k.Parse(sargs))

	if *argVersion == true {
		fmt.Println(Version)
		os.Exit(0)
	}

	conf, err := loadConf(*argConfPath, stdin)
	if err != nil {
		return nil, err
	}

	if conf.ReadTimeout == 0 {
		conf.ReadTimeout = 5 * time.Second
	}
	if conf.WriteTimeout == 0 {
		conf.WriteTimeout = 5 * time.Second
	}

	if len(conf.Protocols) == 0 {
		conf.Protocols = []string{"udp", "tcp"}
	}
	protocols := make(map[streamProtocol]struct{})
	for _, proto := range conf.Protocols {
		switch proto {
		case "udp":
			protocols[_STREAM_PROTOCOL_UDP] = struct{}{}

		case "tcp":
			protocols[_STREAM_PROTOCOL_TCP] = struct{}{}

		default:
			return nil, fmt.Errorf("unsupported protocol: %s", proto)
		}
	}
	if len(protocols) == 0 {
		return nil, fmt.Errorf("no protocols provided")
	}

	if conf.RtspPort == 0 {
		conf.RtspPort = 8554
	}
	if conf.RtpPort == 0 {
		conf.RtpPort = 8000
	}
	if (conf.RtpPort % 2) != 0 {
		return nil, fmt.Errorf("rtp port must be even")
	}
	if conf.RtcpPort == 0 {
		conf.RtcpPort = 8001
	}
	if conf.RtcpPort != (conf.RtpPort + 1) {
		return nil, fmt.Errorf("rtcp and rtp ports must be consecutive")
	}

	if len(conf.Paths) == 0 {
		conf.Paths = map[string]*ConfPath{
			"all": {},
		}
	}

	p := &program{
		conf:       conf,
		protocols:  protocols,
		clients:    make(map[*serverClient]struct{}),
		publishers: make(map[string]publisher),
		events:     make(chan programEvent),
		done:       make(chan struct{}),
	}

	for path, pconf := range conf.Paths {
		if pconf.Source == "" {
			pconf.Source = "record"
		}

		if pconf.PublishUser != "" {
			if !regexp.MustCompile("^[a-zA-Z0-9]+$").MatchString(pconf.PublishUser) {
				return nil, fmt.Errorf("publish username must be alphanumeric")
			}
		}
		if pconf.PublishPass != "" {
			if !regexp.MustCompile("^[a-zA-Z0-9]+$").MatchString(pconf.PublishPass) {
				return nil, fmt.Errorf("publish password must be alphanumeric")
			}
		}
		pconf.publishIps, err = parseIpCidrList(pconf.PublishIps)
		if err != nil {
			return nil, err
		}

		if pconf.ReadUser != "" && pconf.ReadPass == "" || pconf.ReadUser == "" && pconf.ReadPass != "" {
			return nil, fmt.Errorf("read username and password must be both filled")
		}
		if pconf.ReadUser != "" {
			if !regexp.MustCompile("^[a-zA-Z0-9]+$").MatchString(pconf.ReadUser) {
				return nil, fmt.Errorf("read username must be alphanumeric")
			}
		}
		if pconf.ReadPass != "" {
			if !regexp.MustCompile("^[a-zA-Z0-9]+$").MatchString(pconf.ReadPass) {
				return nil, fmt.Errorf("read password must be alphanumeric")
			}
		}
		if pconf.ReadUser != "" && pconf.ReadPass == "" || pconf.ReadUser == "" && pconf.ReadPass != "" {
			return nil, fmt.Errorf("read username and password must be both filled")
		}
		pconf.readIps, err = parseIpCidrList(pconf.ReadIps)
		if err != nil {
			return nil, err
		}

		if pconf.Source != "record" {
			if path == "all" {
				return nil, fmt.Errorf("path 'all' cannot have a RTSP source")
			}

			if pconf.SourceProtocol == "" {
				pconf.SourceProtocol = "udp"
			}

			s, err := newStreamer(p, path, pconf.Source, pconf.SourceProtocol)
			if err != nil {
				return nil, err
			}

			p.streamers = append(p.streamers, s)
			p.publishers[path] = s
		}
	}

	p.log("rtsp-simple-server %s", Version)

	if conf.Pprof {
		go func(mux *http.ServeMux) {
			server := &http.Server{
				Addr:    ":9999",
				Handler: mux,
			}
			p.log("pprof is available on :9999")
			panic(server.ListenAndServe())
		}(http.DefaultServeMux)
		http.DefaultServeMux = http.NewServeMux()
	}

	p.udplRtp, err = newServerUdpListener(p, conf.RtpPort, _TRACK_FLOW_RTP)
	if err != nil {
		return nil, err
	}

	p.udplRtcp, err = newServerUdpListener(p, conf.RtcpPort, _TRACK_FLOW_RTCP)
	if err != nil {
		return nil, err
	}

	p.tcpl, err = newServerTcpListener(p)
	if err != nil {
		return nil, err
	}

	go p.udplRtp.run()
	go p.udplRtcp.run()
	go p.tcpl.run()
	for _, s := range p.streamers {
		go s.run()
	}
	go p.run()

	return p, nil
}

func (p *program) log(format string, args ...interface{}) {
	log.Printf("[%d/%d/%d] "+format, append([]interface{}{len(p.clients),
		p.publisherCount, p.receiverCount}, args...)...)
}

func (p *program) run() {
outer:
	for rawEvt := range p.events {
		switch evt := rawEvt.(type) {
		case programEventClientNew:
			c := newServerClient(p, evt.nconn)
			p.clients[c] = struct{}{}
			c.log("connected")

		case programEventClientClose:
			// already deleted
			if _, ok := p.clients[evt.client]; !ok {
				close(evt.done)
				continue
			}

			delete(p.clients, evt.client)

			if evt.client.path != "" {
				if pub, ok := p.publishers[evt.client.path]; ok && pub == evt.client {
					delete(p.publishers, evt.client.path)

					// if the publisher has disconnected and was ready
					// close all other clients that share the same path
					if pub.publisherIsReady() {
						for oc := range p.clients {
							if oc.path == evt.client.path {
								go oc.close()
							}
						}
					}
				}
			}

			switch evt.client.state {
			case _CLIENT_STATE_PLAY:
				p.receiverCount -= 1

			case _CLIENT_STATE_RECORD:
				p.publisherCount -= 1
			}

			evt.client.log("disconnected")
			close(evt.done)

		case programEventClientDescribe:
			pub, ok := p.publishers[evt.path]
			if !ok || !pub.publisherIsReady() {
				evt.res <- nil
				continue
			}

			evt.res <- pub.publisherSdpText()

		case programEventClientAnnounce:
			_, ok := p.publishers[evt.path]
			if ok {
				evt.res <- fmt.Errorf("someone is already publishing on path '%s'", evt.path)
				continue
			}

			evt.client.path = evt.path
			evt.client.state = _CLIENT_STATE_ANNOUNCE
			p.publishers[evt.path] = evt.client
			evt.res <- nil

		case programEventClientSetupPlay:
			pub, ok := p.publishers[evt.path]
			if !ok || !pub.publisherIsReady() {
				evt.res <- fmt.Errorf("no one is streaming on path '%s'", evt.path)
				continue
			}

			sdpParsed := pub.publisherSdpParsed()

			if len(evt.client.streamTracks) >= len(sdpParsed.Medias) {
				evt.res <- fmt.Errorf("all the tracks have already been setup")
				continue
			}

			evt.client.path = evt.path
			evt.client.streamProtocol = evt.protocol
			evt.client.streamTracks = append(evt.client.streamTracks, &track{
				rtpPort:  evt.rtpPort,
				rtcpPort: evt.rtcpPort,
			})
			evt.client.state = _CLIENT_STATE_PRE_PLAY
			evt.res <- nil

		case programEventClientSetupRecord:
			evt.client.streamProtocol = evt.protocol
			evt.client.streamTracks = append(evt.client.streamTracks, &track{
				rtpPort:  evt.rtpPort,
				rtcpPort: evt.rtcpPort,
			})
			evt.client.state = _CLIENT_STATE_PRE_RECORD
			evt.res <- nil

		case programEventClientPlay1:
			pub, ok := p.publishers[evt.client.path]
			if !ok || !pub.publisherIsReady() {
				evt.res <- fmt.Errorf("no one is streaming on path '%s'", evt.client.path)
				continue
			}

			sdpParsed := pub.publisherSdpParsed()

			if len(evt.client.streamTracks) != len(sdpParsed.Medias) {
				evt.res <- fmt.Errorf("not all tracks have been setup")
				continue
			}

			evt.res <- nil

		case programEventClientPlay2:
			p.receiverCount += 1
			evt.client.state = _CLIENT_STATE_PLAY
			evt.res <- nil

		case programEventClientPause:
			p.receiverCount -= 1
			evt.client.state = _CLIENT_STATE_PRE_PLAY
			evt.res <- nil

		case programEventClientRecord:
			p.publisherCount += 1
			evt.client.state = _CLIENT_STATE_RECORD
			evt.res <- nil

		case programEventClientFrameUdp:
			// find publisher and track id from ip and port
			cl, trackId := func() (*serverClient, int) {
				for _, pub := range p.publishers {
					cl, ok := pub.(*serverClient)
					if !ok {
						continue
					}

					if cl.streamProtocol != _STREAM_PROTOCOL_UDP ||
						cl.state != _CLIENT_STATE_RECORD ||
						!cl.ip().Equal(evt.addr.IP) {
						continue
					}

					for i, t := range cl.streamTracks {
						if evt.trackFlowType == _TRACK_FLOW_RTP {
							if t.rtpPort == evt.addr.Port {
								return cl, i
							}
						} else {
							if t.rtcpPort == evt.addr.Port {
								return cl, i
							}
						}
					}
				}
				return nil, -1
			}()
			if cl == nil {
				continue
			}

			cl.udpLastFrameTime = time.Now()
			p.forwardTrack(cl.path, trackId, evt.trackFlowType, evt.buf)

		case programEventClientFrameTcp:
			p.forwardTrack(evt.path, evt.trackId, evt.trackFlowType, evt.buf)

		case programEventStreamerReady:
			evt.streamer.ready = true
			p.publisherCount += 1
			evt.streamer.log("ready")

		case programEventStreamerNotReady:
			evt.streamer.ready = false
			p.publisherCount -= 1
			evt.streamer.log("not ready")

			// close all clients that share the same path
			for oc := range p.clients {
				if oc.path == evt.streamer.path {
					go oc.close()
				}
			}

		case programEventStreamerFrame:
			p.forwardTrack(evt.streamer.path, evt.trackId, evt.trackFlowType, evt.buf)

		case programEventTerminate:
			break outer
		}
	}

	go func() {
		for rawEvt := range p.events {
			switch evt := rawEvt.(type) {
			case programEventClientClose:
				close(evt.done)

			case programEventClientDescribe:
				evt.res <- nil

			case programEventClientAnnounce:
				evt.res <- fmt.Errorf("terminated")

			case programEventClientSetupPlay:
				evt.res <- fmt.Errorf("terminated")

			case programEventClientSetupRecord:
				evt.res <- fmt.Errorf("terminated")

			case programEventClientPlay1:
				evt.res <- fmt.Errorf("terminated")

			case programEventClientPlay2:
				evt.res <- fmt.Errorf("terminated")

			case programEventClientPause:
				evt.res <- fmt.Errorf("terminated")

			case programEventClientRecord:
				evt.res <- fmt.Errorf("terminated")
			}
		}
	}()

	for _, s := range p.streamers {
		s.close()
	}

	p.tcpl.close()
	p.udplRtcp.close()
	p.udplRtp.close()

	for c := range p.clients {
		c.close()
	}

	close(p.events)
	close(p.done)
}

func (p *program) close() {
	p.events <- programEventTerminate{}
	<-p.done
}

func (p *program) forwardTrack(path string, id int, trackFlowType trackFlowType, frame []byte) {
	for c := range p.clients {
		if c.path == path && c.state == _CLIENT_STATE_PLAY {
			if c.streamProtocol == _STREAM_PROTOCOL_UDP {
				if trackFlowType == _TRACK_FLOW_RTP {
					p.udplRtp.write(&net.UDPAddr{
						IP:   c.ip(),
						Zone: c.zone(),
						Port: c.streamTracks[id].rtpPort,
					}, frame)

				} else {
					p.udplRtcp.write(&net.UDPAddr{
						IP:   c.ip(),
						Zone: c.zone(),
						Port: c.streamTracks[id].rtcpPort,
					}, frame)
				}

			} else {
				c.writeFrame(trackToInterleavedChannel(id, trackFlowType), frame)
			}
		}
	}
}

func main() {
	_, err := newProgram(os.Args[1:], os.Stdin)
	if err != nil {
		log.Fatal("ERR: ", err)
	}

	select {}
}
