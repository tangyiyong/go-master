package master

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/user"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	stateFd       = 5
	listenFdStart = 6
)

var (
	listenFdCount int = 1
	confPath      string
	sockType      string
	services      string
	privilege     bool = false
	verbose       bool = false
	chrootOn      bool = false
)

type PreJailFunc func()
type InitFunc func()
type ExitFunc func()

var (
	preJailHandler PreJailFunc = nil
	initHandler    InitFunc    = nil
	exitHandler    ExitFunc    = nil
	doneChan       chan bool   = make(chan bool)
	connCount      int         = 0
	connMutex      sync.RWMutex
	stopping       bool = false
	waitExit       int  = 10
	prepareCalled  bool = false
)

var (
	logPath    string
	username   string
	masterArgs string
	rootDir    string
)

var (
	MasterConfigure    string
	MasterServiceName  string
	MasterServiceType  string
	MasterVerbose      bool
	MasterUnprivileged bool
	//	MasterChroot       bool
	MasterSocketCount int = 1
	Alone             bool
)

func init() {
	flag.StringVar(&MasterConfigure, "f", "", "app configure file")
	flag.StringVar(&MasterServiceName, "n", "", "app service name")
	flag.StringVar(&MasterServiceType, "t", "sock", "app service type")
	flag.BoolVar(&Alone, "alone", false, "stand alone running")
	flag.BoolVar(&MasterVerbose, "v", false, "app verbose")
	flag.BoolVar(&MasterUnprivileged, "u", false, "app unprivileged")
	//	flag.BoolVar(&MasterChroot, "c", false, "app chroot")
	flag.IntVar(&MasterSocketCount, "s", 1, "listen fd count")
}

func parseArgs() {
	var n = len(os.Args)
	for i := 0; i < n; i++ {
		switch os.Args[i] {
		case "-s":
			i++
			if i < n {
				listenFdCount, _ = strconv.Atoi(os.Args[i])
				if listenFdCount <= 0 {
					listenFdCount = 1
				}
			}
		case "-f":
			i++
			if i < n {
				confPath = os.Args[i]
			}
		case "-t":
			i++
			if i < n {
				sockType = os.Args[i]
			}
		case "-n":
			i++
			if i < n {
				services = os.Args[i]
			}
		case "-u":
			privilege = true
		case "-v":
			verbose = true
		case "-c":
			chrootOn = true
		}
	}

	log.Printf("listenFdCount=%d, sockType=%s, services=%s",
		listenFdCount, sockType, services)
}

func Prepare() {
	if prepareCalled {
		return
	} else {
		prepareCalled = true
	}

	parseArgs()

	conf := new(Config)
	conf.InitConfig(confPath)

	logPath = conf.Get("master_log")
	if len(logPath) > 0 {
		f, err := os.OpenFile(logPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
		if err != nil {
			fmt.Printf("OpenFile %s error %s", logPath, err)
		} else {
			log.SetOutput(f)
			//log.SetOutput(io.MultiWriter(os.Stderr, f))
		}
	}

	masterArgs = conf.Get("master_args")
	username = conf.Get("fiber_owner")
	rootDir = conf.Get("fiber_queue_dir")

	log.Printf("Args: %s\r\n", masterArgs)
}

func chroot() {
	if len(masterArgs) == 0 || !privilege || len(username) == 0 {
		return
	}

	user, err := user.Lookup(username)
	if err != nil {
		log.Printf("Lookup %s error %s", username, err)
	} else {
		gid, err := strconv.Atoi(user.Gid)
		if err != nil {
			log.Printf("invalid gid=%s, %s", user.Gid, err)
		} else if err := syscall.Setgid(gid); err != nil {
			log.Printf("Setgid error %s", err)
		} else {
			log.Printf("Setgid ok")
		}

		uid, err := strconv.Atoi(user.Uid)
		if err != nil {
			log.Printf("invalid uid=%s, %s", user.Uid, err)
		} else if err := syscall.Setuid(uid); err != nil {
			log.Printf("Setuid error %s", err)
		} else {
			log.Printf("Setuid ok")
		}
	}

	if chrootOn && len(rootDir) > 0 {
		err := syscall.Chroot(rootDir)
		if err != nil {
			log.Printf("Chroot error %s, path %s", err, rootDir)
		} else {
			log.Printf("Chroot ok, path %s", rootDir)
			err := syscall.Chdir("/")
			if err != nil {
				log.Printf("Chdir error %s", err)
			} else {
				log.Printf("Chdir ok")
			}
		}
	}
}

func getListenersByAddrs(addrs []string) []*net.Listener {
	listeners := []*net.Listener(nil)
	for _, addr := range addrs {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			panic(fmt.Sprintf("listen error=\"%s\", addr=%s", err, addr))
		}
		listeners = append(listeners, &ln)
	}
	return listeners
}

func getListeners() []*net.Listener {
	listeners := []*net.Listener(nil)
	for fd := listenFdStart; fd < listenFdStart+listenFdCount; fd++ {
		file := os.NewFile(uintptr(fd), "open one listenfd")
		ln, err := net.FileListener(file)
		if err != nil {
			file.Close()
			log.Println(fmt.Sprintf("create FileListener error=\"%s\", fd=%d", err, fd))
			continue
		}
		listeners = append(listeners, &ln)
		log.Printf("add fd: %d", fd)
	}
	return listeners
}

func monitorMaster(listeners []*net.Listener,
	onStopHandler func(), stopHandler func(bool)) {

	file := os.NewFile(uintptr(stateFd), "")
	conn, err := net.FileConn(file)
	if err != nil {
		log.Println("FileConn error", err)
	}

	log.Println("waiting master exiting ...")

	buf := make([]byte, 1024)
	_, err = conn.Read(buf)
	if err != nil {
		log.Println("disconnected from master", err)
	}

	var n, i int
	n = 0
	i = 0

	stopping = true

	if onStopHandler != nil {
		onStopHandler()
	} else {
		// XXX: force stopping listen again
		for _, ln := range listeners {
			log.Println("Closing one listener")
			(*ln).Close()
		}
	}

	for {
		connMutex.RLock()
		if connCount <= 0 {
			connMutex.RUnlock()
			break
		}

		n = connCount
		connMutex.RUnlock()
		time.Sleep(time.Second) // sleep 1 second
		i++
		log.Printf("exiting, clients=%d, sleep=%d seconds", n, i)
		if waitExit > 0 && i >= waitExit {
			log.Printf("waiting too long >= %d", waitExit)
			break
		}
	}

	log.Println("master disconnected, exiting now")

	stopHandler(true)
}

func connCountInc() {
	connMutex.Lock()
	connCount++
	connMutex.Unlock()
}

func connCountDec() {
	connMutex.Lock()
	connCount--
	connMutex.Unlock()
}

func connCountCur() int {
	connMutex.RLock()
	n := connCount
	connMutex.RUnlock()
	return n
}

func OnPreJail(handler PreJailFunc) {
	preJailHandler = handler
}

func OnInit(handler InitFunc) {
	initHandler = handler
}

func OnExit(handler ExitFunc) {
	exitHandler = handler
}
