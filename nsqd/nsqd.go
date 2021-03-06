package nsqd

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nsq/internal/clusterinfo"
	"nsq/internal/dirlock"
	"nsq/internal/http_api"
	"nsq/internal/protocol"
	"nsq/internal/statsd"
	"nsq/internal/util"
	"nsq/internal/version"
)

const (
	TLSNotRequired = iota
	TLSRequiredExceptHTTP
	TLSRequired
)

type errStore struct {
	err error
}

type Client interface {
	Stats() ClientStats
	IsProducer() bool
}

type NSQD struct {
	// 64bit atomic vars need to be first for proper alignment on 32bit platforms
	clientIDSequence int64 // nsqd 借助其为订阅的client生成 ID

	sync.RWMutex //此处组合了锁，在读写和创建topic的时候，用到其方法RLock和RUnlock

	opts atomic.Value //配置的结构体实例

	dl        *dirlock.DirLock //这个文件锁貌似只在linux中用到。
	isLoading int32            //nsqd 当前是否处于启动加载过程。这个也用于原子操作，但是他是基本类型，不需要再被atomic.Value包一下。
	errValue  atomic.Value     // 表示健康状况的错误值
	startTime time.Time        //记录这个实例生成的时间
	//一个nsqd实例可以有多个Topic,使用sync.RWMutex加锁
	topicMap map[string]*Topic //一个NSQD中对应多个Topic集合，string表示的是Topic名称。

	clientLock sync.RWMutex
	clients    map[int64]Client //标识符id对应的client，来一个client就存储一下。存的是订阅了此nsqd所维护的topic的客户端实体

	lookupPeers atomic.Value //需要并发保护的变量，// nsqjd与nsqlookupd之间网络连接抽象实体

	tcpListener   net.Listener //同样，一个NSQD实例有3个这种服务
	httpListener  net.Listener
	httpsListener net.Listener
	tlsConfig     *tls.Config

	poolSize int // queueScanWorker的数量，每个 queueScanWorker代表一个单独的goroutine，用于处理消息队列

	notifyChan           chan interface{}      //当channel或topic更新时（新增或删除），通知nsqlookupd服务更新对应的注册信息
	optsNotificationChan chan struct{}         // 当 nsqd 的配置发生变更时，可以通过此 channel 通知
	exitChan             chan int              // nsqd 退出开关
	waitGroup            util.WaitGroupWrapper // 等待goroutine退出

	ci *clusterinfo.ClusterInfo
}

func New(opts *Options) (*NSQD, error) {
	var err error

	dataPath := opts.DataPath //数据保存地址
	if opts.DataPath == "" {  // 为空就选择当前目录
		cwd, _ := os.Getwd() //获取当前目录，类似Linux的pwd命令
		dataPath = cwd
	}
	if opts.Logger == nil {
		//日志输出大多都是stderr标准错误输出, stderr无缓冲实时性强,stderr本来就是为了输出日志、错误信息之类的运行数据而存在的
		//LstdFlags= Ldate | Ltime,Lmicroseconds表示把时间的毫秒部分也输出出来
		//注意此处，无论谁实现了接口中的方法都可以赋值给这个接口类型，由于是指针实现了opts.Logger的output函数，所以就不能将*log.New赋值给右边，只能将log.New赋值给右边。
		//log.New返回的是标准库的logger
		//我们要接收log.New得到的对象，此处是自定义了一个interface的对象opts.Logger，注意这种写法。
		opts.Logger = log.New(os.Stderr, opts.LogPrefix, log.Ldate|log.Ltime|log.Lmicroseconds) //opts.Logger在n.logf中被用到
	}
	//记录以下当前时间和文件路径并把所有的map/chan都初始化一下。
	n := &NSQD{
		startTime:            time.Now(),
		topicMap:             make(map[string]*Topic), //make和new的功能相似都是分配空间，但是make只能用在slice/map/chan上
		clients:              make(map[int64]Client),  //标识符id对应的client，来一个client就存储一下。存的是订阅了此nsqd所维护的topic的客户端实体
		exitChan:             make(chan int),
		notifyChan:           make(chan interface{}),
		optsNotificationChan: make(chan struct{}, 1),
		dl:                   dirlock.New(dataPath),
	}
	//如果我们创建的客户端所有属性都用默认值的话可用httpcli:=&http.Client{}
	httpcli := http_api.NewClient(nil, opts.HTTPClientConnectTimeout, opts.HTTPClientRequestTimeout) //设置http连接的超时时间，没有用默认的。
	n.ci = clusterinfo.New(n.logf, httpcli)                                                          //第一个参数是一个logf函数的指针，现在先存放到nsqd结构体中的ci元素上，以后在使用这个函数的时候传入参数就行了，比如162行的n.logf（，，）。

	n.lookupPeers.Store([]*lookupPeer{}) //n.lookupPeers是atomic.Value类型，存放需要做并发保护的变量
	n.swapOpts(opts)                     //原子操作，把新的opts存到NSQD.opts这个变量中，其实是替换NSQD中的opts变量。
	n.errValue.Store(errStore{})

	err = n.dl.Lock() //linux中会用到。锁定数据目录（Exit函数中解锁）
	if err != nil {   //失败（锁不上）就退出，说明其他实例在访问
		return nil, fmt.Errorf("--data-path=%s in use (possibly by another instance of nsqd)", dataPath)
	}
	// 最大的压缩比率等级，值越高压缩率越好，但是 CPU 负载也高。
	if opts.MaxDeflateLevel < 1 || opts.MaxDeflateLevel > 9 {
		return nil, errors.New("--max-deflate-level must be [1,9]")
	}
	// work-id范围是[0,1024)
	if opts.ID < 0 || opts.ID >= 1024 {
		return nil, errors.New("--node-id must be [0,1024)")
	}
	//配置推送数据到指定的 statsd , nsqd就会发生对应的 nsqd.*的统计数据到stats.
	//statsd 有四种指标类型：counter计数器、timer计时器、gauge标量和set。
	if opts.StatsdPrefix != "" {
		var port string
		_, port, err = net.SplitHostPort(opts.HTTPAddress)
		if err != nil {
			return nil, fmt.Errorf("failed to parse HTTP address (%s) - %s", opts.HTTPAddress, err)
		}
		statsdHostKey := statsd.HostKey(net.JoinHostPort(opts.BroadcastAddress, port))
		prefixWithHost := strings.Replace(opts.StatsdPrefix, "%s", statsdHostKey, -1)
		if prefixWithHost[len(prefixWithHost)-1] != '.' {
			prefixWithHost += "."
		}
		opts.StatsdPrefix = prefixWithHost
	}
	//TLS和SSL都是在应用层和传输层之间对数据加密，确保传输安全。
	//HTTPS，也称作HTTP over TLS。TLS的前身是SSL。
	if opts.TLSClientAuthPolicy != "" && opts.TLSRequired == TLSNotRequired {
		opts.TLSRequired = TLSRequired
	}

	tlsConfig, err := buildTLSConfig(opts)
	if err != nil {
		return nil, fmt.Errorf("failed to build TLS config - %s", err)
	}
	if tlsConfig == nil && opts.TLSRequired != TLSNotRequired {
		return nil, errors.New("cannot require TLS client connections without TLS key and cert")
	}
	n.tlsConfig = tlsConfig

	for _, v := range opts.E2EProcessingLatencyPercentiles {
		if v <= 0 || v > 1 {
			return nil, fmt.Errorf("invalid E2E processing latency percentile: %v", v)
		}
	}

	n.logf(LOG_INFO, version.String("nsqd")) //由opt中的Logger规定打印到何出。
	n.logf(LOG_INFO, "ID: %d", opts.ID)
	//listen只是注册，accpet对应的才是客户端的Dial，建立连接后开始read和write
	n.tcpListener, err = net.Listen("tcp", opts.TCPAddress) //创建n.tcpListener，后续在这个listener上accept,accept的时候就是等待客户端连接（可阻塞或非阻塞）。
	if err != nil {
		return nil, fmt.Errorf("listen (%s) failed - %s", opts.TCPAddress, err)
	}
	n.httpListener, err = net.Listen("tcp", opts.HTTPAddress)
	if err != nil {
		return nil, fmt.Errorf("listen (%s) failed - %s", opts.HTTPAddress, err)
	}
	if n.tlsConfig != nil && opts.HTTPSAddress != "" {
		n.httpsListener, err = tls.Listen("tcp", opts.HTTPSAddress, n.tlsConfig) //443
		if err != nil {
			return nil, fmt.Errorf("listen (%s) failed - %s", opts.HTTPSAddress, err)
		}
	}

	return n, nil
}

func (n *NSQD) getOpts() *Options {
	return n.opts.Load().(*Options) //从线程安全的n.opts中读取上一步存放的内容。这个n.opts在前面创建NSQD的时候被原子操作存入了。
}

func (n *NSQD) swapOpts(opts *Options) {
	n.opts.Store(opts)
}

func (n *NSQD) triggerOptsNotification() {
	select {
	case n.optsNotificationChan <- struct{}{}:
	default:
	}
}

func (n *NSQD) RealTCPAddr() *net.TCPAddr {
	return n.tcpListener.Addr().(*net.TCPAddr)
}

func (n *NSQD) RealHTTPAddr() *net.TCPAddr {
	return n.httpListener.Addr().(*net.TCPAddr)
}

func (n *NSQD) RealHTTPSAddr() *net.TCPAddr {
	return n.httpsListener.Addr().(*net.TCPAddr)
}

func (n *NSQD) SetHealth(err error) {
	n.errValue.Store(errStore{err: err})
}

func (n *NSQD) IsHealthy() bool {
	return n.GetError() == nil
}

func (n *NSQD) GetError() error {
	errValue := n.errValue.Load()
	return errValue.(errStore).err
}

func (n *NSQD) GetHealth() string {
	err := n.GetError()
	if err != nil {
		return fmt.Sprintf("NOK - %s", err)
	}
	return "OK"
}

func (n *NSQD) GetStartTime() time.Time {
	return n.startTime
}
func (n *NSQD) AddClient(clientID int64, client Client) {
	n.clientLock.Lock()
	n.clients[clientID] = client
	n.clientLock.Unlock()
}

func (n *NSQD) RemoveClient(clientID int64) {
	n.clientLock.Lock()
	_, ok := n.clients[clientID]
	if !ok {
		n.clientLock.Unlock()
		return
	}
	delete(n.clients, clientID)
	n.clientLock.Unlock()
}

func (n *NSQD) Main() error {
	ctx := &context{n}

	exitCh := make(chan error) // 2. 同 NSQLookupd 类似，构建一个退出 hook 函数，且在退出时仅执行一次
	var once sync.Once
	exitFunc := func(err error) {
		once.Do(func() {
			if err != nil {
				n.logf(LOG_FATAL, "%s", err)
			}
			exitCh <- err //对应288行
		})
	}
	//tcp服务可以当生产者发消息也可以当消费者订阅消息，http服务可以用来当生产者发消息（不可以订阅）还可以提供给nsqadmin获取该nsqd本地topic和channel信息
	tcpServer := &tcpServer{ctx: ctx}
	n.waitGroup.Wrap(func() {
		exitFunc(protocol.TCPServer(n.tcpListener, tcpServer, n.logf)) //tcp服务，4150端口，tcp的处理函数和nsqlookupd中的不一样。它可以PUB
	})
	//注意：下面实现了如何根据listen句柄来构建http服务
	httpServer := newHTTPServer(ctx, false, n.getOpts().TLSRequired == TLSRequired)
	n.waitGroup.Wrap(func() {
		exitFunc(http_api.Serve(n.httpListener, httpServer, "HTTP", n.logf)) //http服务。可以PUB
	})
	if n.tlsConfig != nil && n.getOpts().HTTPSAddress != "" {
		httpsServer := newHTTPServer(ctx, true, true) //参数都为true
		n.waitGroup.Wrap(func() {                     //它也能在第三个端口监听 HTTPS
			exitFunc(http_api.Serve(n.httpsListener, httpsServer, "HTTPS", n.logf)) //https服务
		})
	}

	n.waitGroup.Wrap(n.queueScanLoop) //用于进行msg重试，作用对象是inflight队列和deferred队列。保证消息“至少投递一次” 是由这个goroutine中的queueScanWorker不断的扫描 InFlightQueue 实现的。
	//in-flight和deffered queue的。在具体的算法上的话参考了redis的随机过期算法。
	n.waitGroup.Wrap(n.lookupLoop)       //处理与nsqlookupd进程的交互。和lookupd建立长连接，每隔15s ping一下lookupd，新增或者删除topic的时候通知到lookupd，新增或者删除channel的时候通知到lookupd，动态的更新options
	if n.getOpts().StatsdAddress != "" { //如果配置了获取nsqd状态统计的接收地址，才会打开这个统计协程。
		n.waitGroup.Wrap(n.statsdLoop) //还有状态统计处理 go routine
	}

	err := <-exitCh
	return err
}

type meta struct {
	Topics []struct {
		Name     string `json:"name"`
		Paused   bool   `json:"paused"`
		Channels []struct {
			Name   string `json:"name"`
			Paused bool   `json:"paused"`
		} `json:"channels"`
	} `json:"topics"`
}

func newMetadataFile(opts *Options) string {
	return path.Join(opts.DataPath, "nsqd.dat") //将任意数量的路径元素拼接为单个路径返回，会自动忽略空格自动添加斜杠。
}

func readOrEmpty(fn string) ([]byte, error) {
	data, err := ioutil.ReadFile(fn) //在DecodeFile函数中也用到此函数。
	if err != nil {                  //读完不为nil，则有错。
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to read metadata from %s - %s", fn, err)
		}
	}
	return data, nil
}

func writeSyncFile(fn string, data []byte) error {
	f, err := os.OpenFile(fn, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}

	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	f.Close()
	return err
}

//读取当前的nsqd.dat文件内容，nsq对于配置文件的写入，都是用先写临时文件然后进行rename，保存了两份，为了方便回滚时用，
// 所以在配置加载的时候也需要读取2个文件，按进行比较， 看newMetadataFile和oldMetadataFile是否一样，
// 如果不一样就报错。 随后json.Unmarshal(data, &m) 格式化json文件内容，循环Topics然后递归扫描其Channels列表。
//在扫描topic和channel的时候，分别会调用 GetTopic， GetChannel， 其实这两个函数如果在判断topic不存在的时候，会创建他，并且跟lookupd进行联系.
func (n *NSQD) LoadMetadata() error {
	atomic.StoreInt32(&n.isLoading, 1) //通过原子操作的方式把1写入n.isLoading，标识元数据正在加载，加载完毕后会把他置为0。
	defer atomic.StoreInt32(&n.isLoading, 0)
	// 1. 构建 metadata 文件全路径， nsqd.dat，并读取文件内容
	fn := newMetadataFile(n.getOpts())

	data, err := readOrEmpty(fn) //读取元数据文件
	if err != nil {
		return err
	}
	if data == nil { //如果文件内容为空，则表明是首次启动，直接返回
		return nil // fresh start
	}

	var m meta //是这个格式
	err = json.Unmarshal(data, &m)
	if err != nil {
		return fmt.Errorf("failed to parse metadata in %s - %s", fn, err)
	}

	for _, t := range m.Topics {
		if !protocol.IsValidTopicName(t.Name) { //验证topic名称是否符合书写规范
			n.logf(LOG_WARN, "skipping creation of invalid topic %s", t.Name)
			continue
		}
		//根据topic或channel的名称获取对应的实例的方法为nsqd.GetTopic和topic.GetChannel方法
		topic := n.GetTopic(t.Name) //获取指向该topic对象的指针(nsqd/topic.go中Topic struct),如果topic不存在，会自动创建一个
		if t.Paused {               //topic暂停使用，标注到topic对象中
			topic.Pause() //nolint设置paused属性，对于topic而言，若paused属性被设置，则它不会将由生产者发布的消息写入到关联的channel的消息队列。
		}
		for _, c := range t.Channels {
			if !protocol.IsValidChannelName(c.Name) {
				n.logf(LOG_WARN, "skipping creation of invalid channel %s", c.Name)
				continue
			}
			channel := topic.GetChannel(c.Name) //获取与该topic关联的channel列表
			if c.Paused {
				channel.Pause() //nolint设置paused属性，对channel而言，若其paused属性被设置，则那些订阅了此channel的客户端不会被推送消息（这点在后面的源码中可以验证）
			}
		}
		topic.Start() //最后调用topic.Start方法向topic.startChan通道中压入一条消息，消息会在topic.messagePump方法中被取出，以表明topic可以开始进入消息队列处理的主循环。
	}
	return nil
}

//持久化当前的topic,channel数据结构，不涉及到数据不封顶持久化. 写入临时文件后改名， 最后的文件就是nsqd.data。
// 文件比较长，主要是为了保证操作安全，做尽量保证原值操作的，函数会写了2次文件，第一次是json 数据文件，写好后重命名。
// 先写入临时文件，然后做一次重命名（os.Rename），这样避免中间出问题只写了一部分数据，rename是原子操作,所以安全，避免不一致性的发生。
//整个nsq中（包括nsqd和nsqlookupd）涉及到数据持久化的过程只有nsqd的元数据的持久化以及nsqd对消息的持久化（通过diskQueue完成），而nsqlookupd则不涉及持久化操作。
func (n *NSQD) PersistMetadata() error { //是加载元数据的逆操作
	// persist metadata about what topics/channels we have, across restarts
	fileName := newMetadataFile(n.getOpts())

	n.logf(LOG_INFO, "NSQ: persisting topic/channel metadata to %s", fileName)
	//获取nsqd实例内存中的topic集合，并递归地将其对应的channel集合保存到文件，且持久化也是通过先写临时文件，再原子性地重命名。
	js := make(map[string]interface{})
	topics := []interface{}{}
	for _, topic := range n.topicMap {
		if topic.ephemeral { //如果是临时的，就不需要被持久化
			continue
		}
		topicData := make(map[string]interface{})
		topicData["name"] = topic.name
		topicData["paused"] = topic.IsPaused()
		channels := []interface{}{}
		topic.Lock()
		for _, channel := range topic.channelMap {
			channel.Lock()
			if channel.ephemeral { // 临时的 channel 不被持久化
				channel.Unlock()
				continue
			}
			channelData := make(map[string]interface{})
			channelData["name"] = channel.name
			channelData["paused"] = channel.IsPaused()
			channels = append(channels, channelData)
			channel.Unlock()
		}
		topic.Unlock()
		topicData["channels"] = channels
		topics = append(topics, topicData)
	}
	js["version"] = version.Binary
	js["topics"] = topics

	data, err := json.Marshal(&js)
	if err != nil {
		return err
	}

	tmpFileName := fmt.Sprintf("%s.%d.tmp", fileName, rand.Int())

	err = writeSyncFile(tmpFileName, data) //写入临时文件
	if err != nil {
		return err
	}
	err = os.Rename(tmpFileName, fileName) //文件重命名，如果fileName已存在，自动替换掉
	if err != nil {
		return err
	}
	// technically should fsync DataPath here

	return nil
}

func (n *NSQD) Exit() {
	if n.tcpListener != nil {
		n.tcpListener.Close()
	}

	if n.httpListener != nil {
		n.httpListener.Close()
	}

	if n.httpsListener != nil {
		n.httpsListener.Close()
	}

	n.Lock()
	err := n.PersistMetadata() //将元数据写入本地磁盘
	if err != nil {
		n.logf(LOG_ERROR, "failed to persist metadata - %s", err)
	}
	n.logf(LOG_INFO, "NSQ: closing topics")
	for _, topic := range n.topicMap { //关闭topics
		topic.Close()
	}
	n.Unlock()

	n.logf(LOG_INFO, "NSQ: stopping subsystems")
	close(n.exitChan)
	n.waitGroup.Wait() //此处等待所有都退出
	n.dl.Unlock()
	n.logf(LOG_INFO, "NSQ: bye")
}

// GetTopic performs a thread safe operation
// to return a pointer to a Topic object (potentially new)
//根据名称获取topic实例，函数会先简单获取一把读锁看topic是否已经存在，如果已经存在直接返回，如果不存在就到后面的创建，初始化流程。
func (n *NSQD) GetTopic(topicName string) *Topic {
	// most likely, we already have this topic, so try read lock first.
	n.RLock() //先用读锁锁着确保topicMap中的内容不被改变，看一下有没有。读锁占用的情况下会阻止写，不会阻止读，多个 goroutine 可以同时获取读锁。
	t, ok := n.topicMap[topicName]
	n.RUnlock()
	if ok { //如果NSQD找到了这个topic
		return t
	}
	//不存在这topc，得new一个了,  所以直接加锁了整个nsqd结构
	n.Lock() //写锁，锁住整个Topic的创建过程。

	t, ok = n.topicMap[topicName]
	if ok { //还有种情况，就在刚才加写锁那一瞬间，有其他协程进来了，他new了一个，所以获取锁后还得判断一下是否存在
		n.Unlock()
		return t
	}
	//下面就开始处理这个topic不存在的情况。
	// 3. 创建删除指定 topic 的回调函数，即在删除指定的 topic 之前，需要做的一些清理工作，比如关闭与此topic所关联的channel，同时判断删除此topic所包含的所有channel
	deleteCallback := func(t *Topic) { //topic实例的删除回调函数
		n.DeleteExistingTopic(t.name) //nolint
	}
	//创建一个topic结构，并且里面初始化好diskqueue, 加入到NSQD的topicmap里面
	//创建topic的时候，会开启消息协程。这个里面会创建topic的messagePump协程接受消息，还会通知lookup加入新的topic，。
	t = NewTopic(topicName, &context{n}, deleteCallback)
	n.topicMap[topicName] = t

	n.Unlock()

	n.logf(LOG_INFO, "TOPIC(%s): created", t.name)
	// topic is created but messagePump not yet started

	// if loading metadata at startup, no lookupd connections yet, topic started after load
	//“原子的”这个形容词就意味着，在这里读取value的值的同时，当前计算机中的任何CPU都不会进行其它的针对此值的读或写操作。这样的约束是受到底层硬件的支持的。
	if atomic.LoadInt32(&n.isLoading) == 1 { //若nsqd正处在启动过程，刚启动的时候LoadMetadata函数会加载
		return t //当正在加载元数据的时候，还没有lookupd连接，所以没法通知lookupd，topic必须在元数据加载完后才行
	}

	// if using lookupd, make a blocking call to get the topics, and immediately create them.
	// this makes sure that any message received is buffered to the right channels
	//若nsqd并非出于启动过程，则还要进一步填充此topic所关联的channel，即nsqd实例向nsqlookupd实例查询指定topic所关联的channel集合（lookupd里面存储所有之前的channel信息），
	// 然后更新topic.channelMap，同时也要更新topic.memoryMsgChan和topoc.backendChan结构（因为channel集合更新了）。
	lookupdHTTPAddrs := n.lookupdHTTPAddrs() //首先拿到nsqlookupd服务的HTTP地址
	if len(lookupdHTTPAddrs) > 0 {
		// 5.1 从指定的 nsqlookupd 及 topic 所获取的 channel 的集合，nsqlookupd 存储所有之前此 topic 创建的 channel 信息，因此需要加载消息。
		channelNames, err := n.ci.GetLookupdTopicChannels(t.name, lookupdHTTPAddrs) //然后根据这个地址拿到channel
		if err != nil {
			n.logf(LOG_WARN, "failed to query nsqlookupd for channels to pre-create for topic %s - %s", t.name, err)
		}
		for _, channelName := range channelNames {
			if strings.HasSuffix(channelName, "#ephemeral") { //临时channel不需要预先创建，用到的时候再创建就行
				continue // do not create ephemeral channel with no consumer client
			}
			// 5.3 根据 channel name获取 channel实例，且有可能是新建.
			// 若是新建了一个 channel，则通知 topic的后台消息协程去处理channel的更新事件
			// 之所以在查询到指定channel的情况下，新建channel是为了保证消息尽可能不被丢失，
			// 比如在 nsq 重启时，需要在重启的时刻创建那些 channel，避免生产者生产的消息
			// 不能被放到 channel 中，因为在这种情况下，只能等待消费者来指定的 channel 中获取消息才会创建。
			t.GetChannel(channelName)
		}
	} else if len(n.getOpts().NSQLookupdTCPAddresses) > 0 {
		n.logf(LOG_ERROR, "no available nsqlookupd to query for channels to pre-create for topic %s", t.name)
	}

	// now that all channels are added, start topic messagePump
	// 6. 启动了消息处理的循环，往 startChan 通道中 push 了一条消息，
	// 此时会内存消息队列 memoryMsgChan，以及持久化的消息队列 backendChan 就开始工作。
	// 即能处理内存中消息更新的的事件了。
	t.Start() //通知Newtopic中的messagePump不要阻塞了，可以开始处理了
	return t
}

// GetExistingTopic gets a topic only if it exists
func (n *NSQD) GetExistingTopic(topicName string) (*Topic, error) {
	n.RLock()
	defer n.RUnlock()
	topic, ok := n.topicMap[topicName]
	if !ok {
		return nil, errors.New("topic does not exist")
	}
	return topic, nil
}

// DeleteExistingTopic removes a topic only if it exists
func (n *NSQD) DeleteExistingTopic(topicName string) error {
	n.RLock()
	topic, ok := n.topicMap[topicName]
	if !ok {
		n.RUnlock()
		return errors.New("topic does not exist")
	}
	n.RUnlock()

	// delete empties all channels and the topic itself before closing
	// (so that we dont leave any messages around)
	//
	// we do this before removing the topic from map below (with no lock)
	// so that any incoming writes will error and not create a new topic
	// to enforce ordering
	topic.Delete()

	n.Lock()
	delete(n.topicMap, topicName)
	n.Unlock()

	return nil
}

func (n *NSQD) Notify(v interface{}) {
	// since the in-memory metadata is incomplete,
	// should not persist metadata while loading it.
	// nsqd will call `PersistMetadata` it after loading
	persist := atomic.LoadInt32(&n.isLoading) == 0 //决定要不要持久化，若已经在加载状态，说明已经是存在的，不需要做持久化。
	n.waitGroup.Wrap(func() {
		// by selecting on exitChan we guarantee that
		// we do not block exit, see issue #123
		select {
		case <-n.exitChan:
		case n.notifyChan <- v: //把新生成的topic或者channel放到notifyChan
			if !persist { //若处于加载状态，则不持久化
				return
			}
			n.Lock()
			err := n.PersistMetadata() //持久化channel和topic
			if err != nil {
				n.logf(LOG_ERROR, "failed to persist metadata - %s", err)
			}
			n.Unlock()
		}
	})
}

// channels returns a flat slice of all channels in all topics
func (n *NSQD) channels() []*Channel {
	var channels []*Channel
	n.RLock()
	for _, t := range n.topicMap {
		t.RLock()
		for _, c := range t.channelMap {
			channels = append(channels, c)
		}
		t.RUnlock()
	}
	n.RUnlock()
	return channels
}

// resizePool adjusts the size of the pool of queueScanWorker goroutines
//
// 	1 <= pool <= min(num * 0.25, QueueScanWorkerPoolMax)
//
// 调整 queueScanWorker 的数量
func (n *NSQD) resizePool(num int, workCh chan *Channel, responseCh chan bool, closeCh chan int) {
	idealPoolSize := int(float64(num) * 0.25) // // 1. 根据 channel 的数量来设置合适的 pool size，默认理想为nsqd的所有channel数 * 1/4,
	if idealPoolSize < 1 {
		idealPoolSize = 1
	} else if idealPoolSize > n.getOpts().QueueScanWorkerPoolMax {
		idealPoolSize = n.getOpts().QueueScanWorkerPoolMax
	}
	// 2. 开启一个循环，直到理想的 pool size 同实际的 pool size 相同才退出。
	// 若理想值更大，则需扩展已有的 queueScanWorker 的数量，即在一个单独的 goroutine 中调用一次 nsqd.queueScanWorker 方法（开启了一个循环）。
	// 反之 需要减少已有的 queueScanWorker 的数量， 即往 closeCh 中 push 一条消息，强制 queueScanWorker goroutine 退出
	for {
		//最开始poolSize是为0的，后面调整poolSize使其值等于idealPoolSize
		if idealPoolSize == n.poolSize { // 当前启动的worker数等于设定的idealPoolSize，不需要任何的增加或减少，那么直接返回，
			break
		} else if idealPoolSize < n.poolSize { // 最开始是不会进入的此if的，因为poolSize为0。如果大于了idealPoolSize，通过closeCh关闭一个worker
			// queueScanWorker 多了, 减少一个
			// 利用 chan 的特性, 向closeCh 推一个消息, 这样 所有的 worCh 就会随机有一个收到这个消息（这是go chan本身的语言特性）, 然后关闭
			// 细节: 这里跟 exitCh 的用法不同, exitCh 是要告知 "所有的" looper 退出, 所以使用的是 close(exitCh) 的用法
			// 而如果想 让其中 一个 退出, 则使用 exitCh <- 1 的用法
			closeCh <- 1 //此处关的是下面if中的协程
			n.poolSize--
		} else { // 如果未达到idealPoolSize，启动worker的goroutine
			// queueScanWorker 少了, 增加一个
			n.waitGroup.Wrap(func() {
				n.queueScanWorker(workCh, responseCh, closeCh) //开一个死循环的ScanWorker协程
			})
			n.poolSize++
		}
	}
}

// queueScanWorker receives work (in the form of a channel) from queueScanLoop
// and processes the deferred and in-flight queues
// 一旦开始工作 (从 workCh 中收到了信号， 即 dirty 的 channel 的数量达到阈值)，
// 则循环处理 in-flight queue 和 deferred queue 中的消息，
// 并将处理结果（即是否是 dirty channel）通过 reponseCh 反馈给 queueScanWorker。
func (n *NSQD) queueScanWorker(workCh chan *Channel, responseCh chan bool, closeCh chan int) {
	for {
		select {
		// 开始处理两个消息队列中的消息
		case c := <-workCh:
			now := time.Now().UnixNano()
			dirty := false
			// 若返回true，则表明　in-flight 优先队列中有存在处理超时的消息，
			// 因此将消息再次写入到内存队列 memoryMsgChan　或 后端持久化　backend
			// 等待消息被重新投递给消费者（重新被加入到 in-flight queue）
			if c.processInFlightQueue(now) { // 实现消息至少被投递一次，目的就是把所有超时的消息再发一遍。
				dirty = true //有超时就是脏的
			}
			// 若返回 true，则表明　deferred 优先队列中存在延时时间已到的消息，
			// 因此需要将此消息从 deferred queue 中移除，
			// 并将消息重新写入到　内存队列 memoryMsgChan　或后端持久化　backend
			// 等待消息被正式投递给消费者 （正式被加入到 in-flight queue）
			if c.processDeferredQueue(now) { // 实现延迟消息队列，也是按超时的堆排序来进行的。
				dirty = true
			}
			// 报告 queueScanLoop 主循环，发现一个 dirty channel
			responseCh <- dirty // 如果有过期消息的存在，则dirty
			//注意这个地方, 跟 之前的close(exitChan) 用法不同
			//这里是启动多个worker, 然后当判断worker太多了, 需要关闭一个多余的worker时
			//给 closeCh <- 1 发个消息, 利用golang chan 随机分发的特性
			//这样就会随机的关闭掉一个 worker, 也就是随机退出一个 queueScanWorker 的 循环
		case <-closeCh: // 退出处理循环，缩减 queueScanWorker 数量时，被调用
			return
		}
	}
}

// queueScanLoop runs in a single goroutine to process in-flight and deferred
// priority queues. It manages a pool of queueScanWorker (configurable max of
// QueueScanWorkerPoolMax (default: 4)) that process channels concurrently.
//
// It copies Redis's probabilistic expiration algorithm: it wakes up every
// QueueScanInterval (default: 100ms) to select a random QueueScanSelectionCount
// (default: 20) channels from a locally cached list (refreshed every
// QueueScanRefreshInterval (default: 5s)).
//
// If either of the queues had work to do the channel is considered "dirty".
//
// If QueueScanDirtyPercent (default: 25%) of the selected channels were dirty,
// the loop continues without sleep.
// 此函数通过动态的调整queueScanWorker的数目来处理in-flight和deffered queue消息
// 它管理了一个 queueScanWork pool，其默认数量为5。queueScanWorker 可以并发地处理 channel。
// 它借鉴了Redis随机化超时的策略，即它每隔QueueScanInterval时间（默认100ms）就从本地的缓存队列中
// 随机选择 QueueScanSelectionCount 个（默认20个） channels。其中 缓存队列每间隔QueueScanRefreshInterval还会被刷新。
func (n *NSQD) queueScanLoop() {
	// 1. 获取随机选择的 channel 的数量，以及队列扫描的时间间隔，及队列刷新时间间隔
	workCh := make(chan *Channel, n.getOpts().QueueScanSelectionCount)
	responseCh := make(chan bool, n.getOpts().QueueScanSelectionCount) //任务结果 队列
	closeCh := make(chan int)                                          // 用来优雅关闭
	// 利用Ticker来定期开始任务和调整worker
	workTicker := time.NewTicker(n.getOpts().QueueScanInterval)           //表示每隔QueueScanInterval的时间（默认100ms），nsqd随机挑选QueueScanSelectionCount数量的channel执行dirty channel的计数统计
	refreshTicker := time.NewTicker(n.getOpts().QueueScanRefreshInterval) //refreshTicker每过QueueScanRefreshInterval时间（默认5s）就调整queueScanWorker pool的大小。之后，queueScanLoop的任务是处理发送中的消息队列(in-flight queue)，以及被延迟发送的消息队列(deferred queue)两个优先级消息队列中的消息。
	// 2. 获取 nsqd 所包含的 channel 集合，一个 topic 包含多个 channel，而一个 nsqd 实例可包含多个 topic 实例
	channels := n.channels()
	n.resizePool(len(channels), workCh, responseCh, closeCh) // 调整queueScanWorker的数量为所有channel数量的4分之一，多退少补
	// 3. 这个循环中的逻辑就是依据配置参数，反复处理 nsqd 所维护的 topic 集合所关联的 channel 中的消息
	// 即循环处理将 channel 从 topic 接收到的消息，发送给订阅了对应的 channel 的客户端
	for {
		select {
		// 3.1 每过 QueueScanInterval 时间（默认100ms），
		// 则开始随机挑选 QueueScanSelectionCount 个 channel。转到 loop: 开始执行
		case <-workTicker.C: // 开始一次任务的派发，若没有channels则continue，若有channels则可以执行select之外的代码
			if len(channels) == 0 {
				continue
			}
			// 3.2 每过 QueueScanRefreshInterval 时间（默认5s），
			// 则调整 pool 的大小，即调整开启的 queueScanWorker 的数量为 pool 的大小
		case <-refreshTicker.C: // 重新调整 worker 数量
			channels = n.channels()
			n.resizePool(len(channels), workCh, responseCh, closeCh)
			continue
		case <-n.exitChan: // 3.3 nsqd 已退出
			goto exit
		}

		num := n.getOpts().QueueScanSelectionCount //很好理解，挑选出来的要处理的channel数不能大于所有channel总数
		if num > len(channels) {
			num = len(channels)
		}
		// 3.4 利用 util.UniqRands，随机选取 num个（QueueScanSelectionCount 默认20个）channel
		// 将它们 push 到 workCh 管道，queueScanWorker 中会收到此消息，
		// 然后立即处理 in-flight queue 和 deferred queue 中的消息。
		// 注意，因为这里是随机抽取 channel 因此，有可能被选中的 channel 中并没有消息
	loop:
		for _, i := range util.UniqRands(num, len(channels)) { //从0~ len(channels)随机取出num个数。随机取出num个channel, 派发给 worker 进行 扫描
			workCh <- channels[i] //此处是怎么保证随机性的？关键就在于n.channels()中在获取channel数组的时候，用的是遍历map，map的特点是遍历拿到的是随机的，数组中各channel是随机排列的。此处若UniqRands的两个参数是固定值，则每次拿到的i也会是固定值，但并不影响channels[i]是随机的。
			//workCh的长度为num,处理workCh的协程数为总channel数的4分之1
		}
		// 接收 扫描结果, 统计有多少channel是"脏"的
		// 3.5 统计 dirty 的 channel 的数量， responseCh管道在上面的 nsqd.resizePool方法中
		// 传递给了 len(channels) * 0.25 个 queueScanWorker。
		// 它们会在循环中反复查看两个消息优先级队列中是否有消息等待被处理：
		// 即查看 inFlightPQ 和 deferredPQ。
		numDirty := 0
		for i := 0; i < num; i++ {
			if <-responseCh { //responseCh的长度和workCh的长度一样，处理一个就有一个responseCh
				numDirty++
			}
		}
		// 假如 "dirty"的"比例"大于配置的QueueScanDirtyPercent(默认为25%）, 则不等待workTicker，马上进行下一轮扫描,目的就是减少需要处理inflight的channel
		if float64(numDirty)/float64(num) > n.getOpts().QueueScanDirtyPercent {
			goto loop
		}
	}

exit:
	n.logf(LOG_INFO, "QUEUESCAN: closing")
	close(closeCh)
	workTicker.Stop()
	refreshTicker.Stop()
}

func buildTLSConfig(opts *Options) (*tls.Config, error) {
	var tlsConfig *tls.Config

	if opts.TLSCert == "" && opts.TLSKey == "" {
		return nil, nil
	}

	tlsClientAuthPolicy := tls.VerifyClientCertIfGiven
	//X.509是一种非常通用的证书格式。
	cert, err := tls.LoadX509KeyPair(opts.TLSCert, opts.TLSKey) //第一步，读取证书和私钥两个文件，并返回一个 tls.Certificate变量
	if err != nil {
		return nil, err
	}
	switch opts.TLSClientAuthPolicy {
	case "require":
		tlsClientAuthPolicy = tls.RequireAnyClientCert
	case "require-verify":
		tlsClientAuthPolicy = tls.RequireAndVerifyClientCert
	default:
		tlsClientAuthPolicy = tls.NoClientCert
	}

	tlsConfig = &tls.Config{ //此为第二步。第3步在443上tls.Listen，第4步，accept
		Certificates: []tls.Certificate{cert}, //把证书填进去
		ClientAuth:   tlsClientAuthPolicy,
		MinVersion:   opts.TLSMinVersion,
		MaxVersion:   tls.VersionTLS12, // enable TLS_FALLBACK_SCSV prior to Go 1.5: https://go-review.googlesource.com/#/c/1776/
	}

	if opts.TLSRootCAFile != "" {
		tlsCertPool := x509.NewCertPool()
		caCertFile, err := ioutil.ReadFile(opts.TLSRootCAFile)
		if err != nil {
			return nil, err
		}
		if !tlsCertPool.AppendCertsFromPEM(caCertFile) {
			return nil, errors.New("failed to append certificate to pool")
		}
		tlsConfig.ClientCAs = tlsCertPool
	}

	tlsConfig.BuildNameToCertificate()

	return tlsConfig, nil
}

func (n *NSQD) IsAuthEnabled() bool {
	return len(n.getOpts().AuthHTTPAddresses) != 0
}
