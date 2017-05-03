package main

import (
	"os"
	"os/signal"
	"syscall"
	"runtime"

	"github.com/zengnotes/redis-proxy/codis/proxy"
	"github.com/zengnotes/redis-proxy/codis/utils/logger"
	"log"
	"time"
	"github.com/zengnotes/redis-proxy/codis/models"
	"strings"
	"github.com/zengnotes/redis-proxy/config"
)
//mac go build -ldflags -s -w -o
func init() {
	runtime.GOMAXPROCS(runtime.NumCPU())
}

func main() {
	conf := config.NewDefaultConfig()
	//文件读取
	if err := conf.LoadFromFile("./bin/proxy.toml"); err != nil {
		log.Panicf("load config %s failed %v", "./bin/proxy.toml", err)
	}
	//日志文件
	conf.InitLogwriter()
	//开始
	s, err := proxy.New(conf)
	if err != nil {
		config.Logger.Logf(logger.ERROR, "create proxy with config file failed\n%s", conf)
	}
	defer s.Close()

	defer config.Logger.Close()
	//TODO 如果是多少 redis集群，随机使用多少
	serverHost := "192.168.0.206:6379,192.168.0.206:6380,192.168.0.206:6381,192.168.0.207:6379," +
		"192.168.0.207:6380,192.168.0.207:6381,192.168.0.208:6379,192.168.0.208:6380,192.168.0.208:6381"
	servers := strings.Split(serverHost, ",")
	slots := make([]*models.Slot, models.MaxSlotNum)
	for i := 0; i < models.MaxSlotNum; i++ {
		slot := &models.Slot{
			Id:          i,
			BackendAddr: servers[i % len(servers)],
		}
		slots[i] = slot
	}
	if slots != nil && len(slots) == models.MaxSlotNum {
		go AutoOnlineWithFillSlots(s, slots)
	}

	for !s.IsClosed() && !s.IsOnline() {
		config.Logger.Logf(logger.WARNING, "[%p] proxy waiting online ...", s)
		time.Sleep(time.Second)
	}

	config.Logger.Logf(logger.CRITICAL, "[%p] proxy is working ...", s)

	for !s.IsClosed() {
		time.Sleep(time.Second)
	}

	config.Logger.Logf(logger.CRITICAL, "[%p] proxy is exiting ...", s)

	go func() {
		defer s.Close()
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGKILL, syscall.SIGTERM)

		sig := <-c
		config.Logger.Logf(logger.CRITICAL, "[%p] proxy receive signal = '%v'", s, sig)
	}()
}

func AutoOnlineWithFillSlots(p *proxy.Proxy, slots []*models.Slot) {
	if err := p.FillSlots(slots); err != nil {
		log.Panic("fill slots failed" + err.Error())
	}
	if err := p.Start(); err != nil {
		log.Panic("start proxy failed" + err.Error())
	}
}