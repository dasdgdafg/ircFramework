package ircFramework

import (
	"bufio"
	"crypto/tls"
	"io"
	"log"
	"net"
	"os"
	"regexp"
	"time"
)

// constant regexes
var pingRegex = regexp.MustCompile("^PING :(.*)$")                              // PING :message
var motdEndRegex = regexp.MustCompile("^[^ ]* (?:376|422)")                     // :server 376 nick :End of /MOTD command.
var privmsgRegex = regexp.MustCompile("^:([^!]*)![^ ]* PRIVMSG ([^ ]*) :(.*)$") // :nick!ident@host PRIVMSG #channel :message

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

	state channelState
	// regexes that depend on the nick
	vhostSetRegex *regexp.Regexp
	inviteRegex   *regexp.Regexp
	kickRegex     *regexp.Regexp
	banRegex      *regexp.Regexp
}

func (b *IRCBot) Run() {
	if b.Statefile == "" {
		log.Println("no statefile, using mybot.state")
		b.Statefile = "mybot.state"
	}
	b.state = channelState{b.Statefile}
	log.Println("starting up")

	b.vhostSetRegex = regexp.MustCompile("^[^ ]* 396 " + b.Nickname)                    // :server 396 nick host :is now your visible host
	b.inviteRegex = regexp.MustCompile("^([^ ]*) INVITE " + b.Nickname + " :(.*)$")     // :nick!ident@host INVITE nick :#channel
	b.kickRegex = regexp.MustCompile("^([^ ]*) KICK ([^ ]*) " + b.Nickname + " :(.*)$") // :nick!ident@host KICK #channel nick :message
	b.banRegex = regexp.MustCompile("^[^ ]* 474 " + b.Nickname + " ([^ ]*) :(.*)$")     // :server 474 nick #channel :Cannot join channel (+b)

	t := time.Now()
	ts := t.Format("Jan 2 2006 15-04-05 ET")

	logFilename := "logs/" + ts + ".txt"
	f, err := os.Create(logFilename)
	if err != nil {
		log.Fatalln(err)
	}
	defer f.Close()
	// output to both stdout and the log file by default
	logWriter := io.MultiWriter(os.Stdout, f)
	log.SetOutput(logWriter)
	fileOnlyLogger := log.New(f, "", log.Flags())

	socket, err := tls.Dial("tcp", b.Server+":"+b.Port, &tls.Config{})
	if err != nil {
		log.Fatalln(err)
	}
	defer socket.Close()
	log.Println("socket connected")

	errors := make(chan bool)
	linesToSend := make(chan string, 5)
	dbWrites := make(chan dbOp, 2)
	go b.readLines(socket, errors, linesToSend, fileOnlyLogger, dbWrites)
	go writeLines(socket, errors, linesToSend, fileOnlyLogger)
	if b.ListenToStdin {
		go manualInput(linesToSend)
	}
	go b.writeToDb(dbWrites)
	linesToSend <- "NICK " + b.Nickname
	linesToSend <- "USER " + b.Ident + " * 8 :" + b.Realname

	if b.ExtraLinesToSend != nil {
		go func() {
			for {
				// send any values on ExtraLinesToSend over to linesToSend
				linesToSend <- (<-b.ExtraLinesToSend)
			}
		}()
	}

	<-errors
}

// ensure db writes are done sequentially
func (b *IRCBot) writeToDb(dbWrites <-chan dbOp) {
	for {
		job := <-dbWrites
		if job.add {
			b.state.AddChannel(job.channel)
		} else {
			b.state.RemoveChannel(job.channel)
		}
	}
}

// listen to stdin and send any lines over the socket
func manualInput(linesToSend chan<- string) {
	reader := bufio.NewReader(os.Stdin)
	for {
		text, err := reader.ReadString('\n')
		if err != nil {
			log.Fatalln(err)
		}
		linesToSend <- text
	}
}

func (b *IRCBot) readLines(socket net.Conn, errors chan<- bool, linesToSend chan<- string, logFile *log.Logger, dbWrites chan<- dbOp) {
	reader := bufio.NewReader(socket)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			log.Println(err)
			errors <- true
			return
		}
		// remove the trailing \r\n
		line = line[:len(line)-2]
		logFile.Println(">>> " + line) // only print these to the log file, not to the default logger
		b.processLine(line, linesToSend, dbWrites)
	}
}

func writeLines(socket net.Conn, errors chan<- bool, linesToSend <-chan string, logFile *log.Logger) {
	writer := bufio.NewWriter(socket)
	for {
		line := <-linesToSend
		logFile.Println("<<< " + line) // only print these to the log file, not to the default logger
		_, err := writer.WriteString(line + "\n")
		if err != nil {
			log.Println(err)
			errors <- true
			return
		}
		// make sure it actually gets sent
		writer.Flush()
	}
}

func (b *IRCBot) processLine(line string, linesToSend chan<- string, dbWrites chan<- dbOp) {
	if pingRegex.MatchString(line) {
		linesToSend <- "PONG :" + pingRegex.FindStringSubmatch(line)[1]
	} else if motdEndRegex.MatchString(line) {
		linesToSend <- "PRIVMSG nickserv :identify " + b.Password
	} else if b.vhostSetRegex.MatchString(line) {
		log.Println("joining channels")
		go b.joinChannels(linesToSend)
	} else if privmsgRegex.MatchString(line) {
		matches := privmsgRegex.FindStringSubmatch(line)
		b.processPrivmsg(linesToSend, matches[1], matches[2], matches[3])
	} else if b.inviteRegex.MatchString(line) {
		matches := b.inviteRegex.FindStringSubmatch(line)
		log.Println("joining " + matches[2] + ", invited by " + matches[1])
		linesToSend <- "JOIN " + matches[2]
		dbWrites <- dbOp{true, matches[2]}
	} else if b.kickRegex.MatchString(line) {
		matches := b.kickRegex.FindStringSubmatch(line)
		log.Println("kicked from " + matches[2] + " by " + matches[1] + " because " + matches[3])
		dbWrites <- dbOp{false, matches[2]}
	} else if b.banRegex.MatchString(line) {
		matches := b.banRegex.FindStringSubmatch(line)
		log.Println("can't join " + matches[1] + " because " + matches[2])
		dbWrites <- dbOp{false, matches[2]}
	}
}

func (b *IRCBot) joinChannels(linesToSend chan<- string) {
	for _, ch := range b.state.GetChannels() {
		linesToSend <- "JOIN " + ch
	}
}

func (b *IRCBot) processPrivmsg(linesToSend chan<- string, nick string, channel string, msg string) {
	if b.MessageHandler != nil {
		b.MessageHandler(linesToSend, nick, channel, msg)
	}
}
