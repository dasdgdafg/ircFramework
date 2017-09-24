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

	state channelState
}

func (b *IRCBot) Run() {
	if b.Statefile == "" {
		log.Println("no statefile, using mybot.state")
		b.Statefile = "mybot.state"
	}
	b.state = channelState{b.Statefile}
	log.Println("starting up")

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
	strs := strings.SplitN(line, " ", 4)
	if len(strs) >= 2 {
		if strs[0] == "PING" {
			// PING :message
			linesToSend <- "PONG :" + afterColon(strs[1])
		} else if strs[1] == "376" || strs[1] == "422" {
			// :server 376 nick :End of /MOTD command.
			linesToSend <- "PRIVMSG nickserv :identify " + b.Password
		} else if strs[1] == "396" && strs[2] == b.Nickname {
			// :server 396 nick host :is now your visible host
			log.Println("joining channels")
			go b.joinChannels(linesToSend)
		} else if strs[1] == "PRIVMSG" {
			// :nick!ident@host PRIVMSG #channel :message
			b.processPrivmsg(linesToSend, extractNick(strs[0]), strs[2], afterColon(strs[3]))
		} else if strs[1] == "INVITE" && strs[2] == b.Nickname {
			// :nick!ident@host INVITE nick :#channel
			channel := afterColon(strs[3])
			log.Println("joining " + channel + ", invited by " + strs[1])
			linesToSend <- "JOIN " + channel
			dbWrites <- dbOp{true, channel}
		} else if strs[1] == "KICK" {
			// :nick!ident@host KICK #channel nick :message
			nickAndMsg := strings.SplitN(strs[3], " ", 1)
			if nickAndMsg[0] == b.Nickname {
				message := ""
				if len(nickAndMsg) > 1 {
					message = nickAndMsg[1]
				}
				log.Println("kicked from " + strs[2] + " by " + strs[0] + " because " + message)
				dbWrites <- dbOp{false, strs[2]}
			}
		} else if strs[1] == "474" && strs[2] == b.Nickname {
			// :server 474 nick #channel :Cannot join channel (+b)
			log.Println("can't join " + strs[3])
			dbWrites <- dbOp{false, strings.Split(strs[3], " ")[0]}
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
