package ircFramework

import (
	"io/ioutil"
	"log"
	"strings"
	"sync"
)

type channelState struct {
	channelFileName string
}

var lock = sync.Mutex{}

func (s *channelState) AddChannel(channel string) {
	lock.Lock()
	defer lock.Unlock()
	channels := s.readData()
	added := false
	for _, ch := range channels {
		if ch == channel {
			log.Println("already added " + channel)
			added = true
			break
		}
	}
	if !added {
		log.Println("adding " + channel)
		channels = append(channels, channel)
		s.writeData(channels)
	}
}

func (s *channelState) RemoveChannel(channel string) {
	lock.Lock()
	defer lock.Unlock()
	channels := s.readData()
	for i, ch := range channels {
		if ch == channel {
			log.Println("removing " + channel)
			channels = append(channels[:i], channels[i+1:]...)
			s.writeData(channels)
			return
		}
	}
	log.Println("don't know about " + channel)
}

func (s *channelState) GetChannels() []string {
	lock.Lock()
	defer lock.Unlock()
	return s.readData()
}

func (s *channelState) readData() []string {
	channelsBytes, err := ioutil.ReadFile(s.channelFileName)
	if err != nil {
		log.Println("failed to get channels ", s.channelFileName)
		log.Println(err)
		return []string{}
	}
	channels := strings.Split(string(channelsBytes), "\n")
	return channels
}

func (s *channelState) writeData(channels []string) {
	channelBytes := []byte(strings.Join(channels, "\n"))
	ioutil.WriteFile(s.channelFileName, channelBytes, 0666)
}
