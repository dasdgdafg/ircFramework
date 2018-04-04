package ircFramework

import (
	"bufio"
	"crypto/tls"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

type dbOp struct {
	add     bool
	channel string
}

type IRCBot struct {
	Server           string
	Port             string
	Nickname         string
	Ident            string
	Realname         string
	Password         string
	Statefile        string
	ListenToStdin    bool
	MessageHandler   func(linesToSend chan<- string, nick string, channel string, msg string)
	ExtraLinesToSend chan string

	state               channelState
	userLinesToSend     chan string
	retryUserLine       string
	internalLinesToSend chan string
	dbWrites            chan dbOp
	logger              *log.Logger
	lastError           time.Time
	retryDelay          time.Duration
}

// default time to wait before reconnecting, increases exponentially
var defaultRetryTimeout = 10 * time.Second

// time to wait for a given read/write operation before assuming the connection is dead
var socketTimeout = 10 * time.Minute

func (b *IRCBot) Run() {
	if b.Statefile == "" {
		log.Println("no statefile, using mybot.state")
		b.Statefile = "mybot.state"
	}
	b.state = channelState{b.Statefile}
	log.Println("starting up")

	t := time.Now()
	ts := t.Format("2006-01-02 15-04-05")

	logFilename := "logs/" + ts + ".txt"
	f, err := os.Create(logFilename)
	if err != nil {
		log.Fatalln(err)
	}
	defer f.Close()
	// output to both stdout and the log file by default
	logWriter := io.MultiWriter(os.Stdout, f)
	log.SetOutput(logWriter)
	b.logger = log.New(f, "", log.Flags())

	b.userLinesToSend = make(chan string, 5)  // lines the user wants to send
	b.internalLinesToSend = make(chan string) // lines generated internally by this framework
	b.dbWrites = make(chan dbOp, 2)

	go b.reconnect()

	if b.ListenToStdin {
		go b.manualInput()
	}
	go b.writeToDb()

	if b.ExtraLinesToSend != nil {
		go func() {
			for {
				// send any values on ExtraLinesToSend over to userLinesToSend
				b.userLinesToSend <- (<-b.ExtraLinesToSend)
			}
		}()
	}

	<-make(chan int)
}

// create a new socket and start read/writing it
func (b *IRCBot) reconnect() {
	kys := make(chan struct{}) // used to signal that the connection is dead and goroutines using it should stop
	socket, err := tls.Dial("tcp", b.Server+":"+b.Port, &tls.Config{})
	if err != nil {
		log.Println(err)
		go b.disconnected(kys, socket)
		return
	}
	log.Println("socket connected")
	// start reading and writing internal lines, but don't write user lines yet because they won't succeed until after we've joined the irc channels
	go b.readLines(kys, socket)
	go b.writeInternalLines(kys, socket)
	// try to send the nick and ident
	select {
	case b.internalLinesToSend <- "NICK " + b.Nickname:
		select {
		case b.internalLinesToSend <- "USER " + b.Ident + " * 8 :" + b.Realname:
		case _, _ = <-kys:
		}
	case _, _ = <-kys:
	}
}

func (b *IRCBot) connected(kys chan struct{}, socket net.Conn) {
	// safe to write the user lines now
	go b.writeUserLines(kys, socket)
}

func (b *IRCBot) disconnected(kys chan struct{}, socket net.Conn) {
	select {
	case _, _ = <-kys:
		// channel already closed, do nothing
		return
	default:
		// channel is still open, close it and the socket
		if socket != nil {
			socket.Close()
		}
		close(kys)
		// retry with exponential backoff
		if !b.lastError.IsZero() {
			// reset the timeout if the connection was working for an hour
			if time.Since(b.lastError) > time.Hour {
				b.retryDelay = defaultRetryTimeout
			} else {
				b.retryDelay *= 2
			}
			log.Println("waiting before reconnecting")
			<-time.After(b.retryDelay)
		}
		b.lastError = time.Now()

		b.reconnect()
	}
}

// ensure db writes are done sequentially
func (b *IRCBot) writeToDb() {
	for {
		job := <-b.dbWrites
		if job.add {
			b.state.AddChannel(job.channel)
		} else {
			b.state.RemoveChannel(job.channel)
		}
	}
}

// listen to stdin and send any lines over the socket
func (b *IRCBot) manualInput() {
	reader := bufio.NewReader(os.Stdin)
	for {
		text, err := reader.ReadString('\n')
		if err != nil {
			log.Fatalln(err)
		}
		b.userLinesToSend <- text
	}
}

func (b *IRCBot) readLines(kys chan struct{}, socket net.Conn) {
	reader := bufio.NewReader(socket)
	for {
		select {
		case _, _ = <-kys:
			return
		default:
			socket.SetReadDeadline(time.Now().Add(socketTimeout))
			line, err := reader.ReadString('\n')
			if err != nil {
				log.Println(err)
				go b.disconnected(kys, socket)
				return
			}
			// remove the trailing \r\n
			line = line[:len(line)-2]
			b.logger.Println(">>> " + line) // only print these to the log file, not to the default logger
			b.processLine(kys, socket, line)
		}
	}
}

func (b *IRCBot) writeUserLines(kys chan struct{}, socket net.Conn) {
	if b.retryUserLine != "" {
		line := b.retryUserLine
		b.logger.Println("(retry) <<< " + line) // only print these to the log file, not to the default logger
		_, err := socket.Write([]byte(line + "\n"))
		if err != nil {
			log.Println(err)
			go b.disconnected(kys, socket)
			return
		}
	}
	for {
		var line string
		select {
		case _, _ = <-kys:
			return
		case line = <-b.userLinesToSend:
		}
		b.logger.Println("<<< " + line) // only print these to the log file, not to the default logger
		_, err := socket.Write([]byte(line + "\n"))
		if err != nil {
			log.Println(err)
			b.retryUserLine = line
			go b.disconnected(kys, socket)
			return
		}
	}
}

func (b *IRCBot) writeInternalLines(kys chan struct{}, socket net.Conn) {
	for {
		var line string
		select {
		case _, _ = <-kys:
			return
		case line = <-b.internalLinesToSend:
		}
		b.logger.Println("<<< " + line) // only print these to the log file, not to the default logger
		_, err := socket.Write([]byte(line + "\n"))
		if err != nil {
			log.Println(err)
			go b.disconnected(kys, socket)
			return
		}
	}
}

func (b *IRCBot) processLine(kys chan struct{}, socket net.Conn, line string) {
	var lineToSend string
	strs := strings.SplitN(line, " ", 4)
	if len(strs) >= 2 {
		if strs[0] == "PING" {
			// PING :message
			lineToSend = "PONG :" + afterColon(strs[1])
		} else if strs[1] == "376" || strs[1] == "422" {
			// :server 376 nick :End of /MOTD command.
			lineToSend = "PRIVMSG nickserv :identify " + b.Password
		} else if strs[1] == "396" && strs[2] == b.Nickname {
			// :server 396 nick host :is now your visible host
			log.Println("joining channels")
			go b.joinChannels(kys, socket)
		} else if strs[1] == "PRIVMSG" {
			// :nick!ident@host PRIVMSG #channel :message
			b.processPrivmsg(extractNick(strs[0]), strs[2], afterColon(strs[3]))
		} else if strs[1] == "INVITE" && strs[2] == b.Nickname {
			// :nick!ident@host INVITE nick :#channel
			channel := afterColon(strs[3])
			log.Println("joining " + channel + ", invited by " + strs[0])
			b.dbWrites <- dbOp{true, channel}
			lineToSend = "JOIN " + channel
		} else if strs[1] == "KICK" {
			// :nick!ident@host KICK #channel nick :message
			nickAndMsg := strings.SplitN(strs[3], " ", 2)
			if nickAndMsg[0] == b.Nickname {
				message := ""
				if len(nickAndMsg) > 1 {
					message = nickAndMsg[1]
				}
				log.Println("kicked from " + strs[2] + " by " + strs[0] + " because " + message)
				b.dbWrites <- dbOp{false, strs[2]}
			}
		} else if strs[1] == "474" && strs[2] == b.Nickname {
			// :server 474 nick #channel :Cannot join channel (+b)
			log.Println("can't join " + strs[3])
			b.dbWrites <- dbOp{false, strings.Split(strs[3], " ")[0]}
		}
	}
	if lineToSend != "" {
		select {
		case _, _ = <-kys:
			return
		case b.internalLinesToSend <- lineToSend:
		}
	}
}

// :nick!ident@host -> nick
func extractNick(nick string) string {
	start := strings.Index(nick, ":")
	end := strings.Index(nick, "!")
	if start >= 0 && end > start {
		return nick[start+1 : end]
	}
	return ""
}

// return the part of the string after the first :
func afterColon(str string) string {
	start := strings.Index(str, ":")
	if start >= 0 {
		return str[start+1:]
	}
	return ""
}

func (b *IRCBot) joinChannels(kys chan struct{}, socket net.Conn) {
	// TODO: check for kys, set connected once done
	for _, ch := range b.state.GetChannels() {
		select {
		case _, _ = <-kys:
			return
		case b.internalLinesToSend <- "JOIN " + ch:
		}
	}
	// now that the irc channels have been joined, we're done setting up the connection
	go b.connected(kys, socket)
}

func (b *IRCBot) processPrivmsg(nick string, channel string, msg string) {
	if b.MessageHandler != nil {
		b.MessageHandler(b.userLinesToSend, nick, channel, msg)
	}
}
