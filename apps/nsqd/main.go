package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/judwhite/go-svc/svc"
	"github.com/mreiferson/go-options"
	"github.com/nsqio/nsq/internal/lg"
	"github.com/nsqio/nsq/internal/version"
	"github.com/nsqio/nsq/nsqd"
)

type program struct {
	once sync.Once
	nsqd *nsqd.NSQD //此处并非组合，组合只能是匿名对象，所以此处就相当于只是记录一下。
}

func main() {
	prg := &program{}
	if err := svc.Run(prg, syscall.SIGINT, syscall.SIGTERM); err != nil { //nsqd也是通过go-svc启动
		logFatal("%s", err)
	}
}

func (p *program) Init(env svc.Environment) error {
	if env.IsWindowsService() {
		dir := filepath.Dir(os.Args[0])
		return os.Chdir(dir)
	}
	return nil
}
//start返回后会进入到svc的代码里面进行等待监听信号量，如果用户杀进程，就调用下面的stop
func (p *program) Start() error {
	opts := nsqd.NewOptions()

	flagSet := nsqdFlagSet(opts)//修改默认配置
	flagSet.Parse(os.Args[1:]) //因为用到了NewFlagSet,所以此处就需要指定Parse的参数，如果用的是默认Flag,则其参数无需指定

	rand.Seed(time.Now().UTC().UnixNano()) //设置随机数种子，后面所有的随机数的操作都是根据这个种子来的。
	if flagSet.Lookup("version").Value.(flag.Getter).Get().(bool) {//flag.Getter无法理解
		fmt.Println(version.String("nsqd"))
		os.Exit(0)
	}

	var cfg config
	configFile := flagSet.Lookup("config").Value.String()
	if configFile != "" {
		_, err := toml.DecodeFile(configFile, &cfg)//
		if err != nil {
			logFatal("failed to load config file %s - %s", configFile, err)
		}
	}
	cfg.Validate() //验证配置是否合法，主要关于TLS的验证

	options.Resolve(opts, flagSet, cfg)//要学习反射，把下面这个函数看懂就行了
	nsqd, err := nsqd.New(opts)
	if err != nil {
		logFatal("failed to instantiate nsqd - %s", err)
	}
	p.nsqd = nsqd

	err = p.nsqd.LoadMetadata()//加载磁盘文件nsqd.data时，会先创建所有之前的topic,初始化n.topics结构
	if err != nil {
		logFatal("failed to load metadata - %s", err)
	}
	//持久化当前的topic,channel数据结构，不涉及到数据不封顶持久化. 写入临时文件后改名
	//怎么刚刚启动就要持久化呢？原因是？ 搞回滚用? 清理之前的回滚信息? 比如之前有失败执行的，后来改了要求的
	err = p.nsqd.PersistMetadata() //持久化数据
	if err != nil {
		logFatal("failed to persist metadata - %s", err)
	}

	go func() {
		err := p.nsqd.Main()//开始监听服务
		if err != nil {
			p.Stop()
			os.Exit(1)
		}
	}()

	return nil
}

func (p *program) Stop() error {
	p.once.Do(func() {
		p.nsqd.Exit()
	})
	return nil
}

func logFatal(f string, args ...interface{}) {
	lg.LogFatal("[nsqd] ", f, args...)
}
