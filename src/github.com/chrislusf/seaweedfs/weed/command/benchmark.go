package command

import (
	"bufio"
	"context"
	"fmt"
	"github.com/chrislusf/seaweedfs/weed/server"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"io"
	"math"
	"math/rand"
	"os"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/operation"
	"github.com/chrislusf/seaweedfs/weed/security"
	"github.com/chrislusf/seaweedfs/weed/util"
	"github.com/chrislusf/seaweedfs/weed/wdclient"
)

//基准，统计信息
//指标值可以动态变化
type BenchmarkOptions struct {
	masters          *string //主地址，默认localhost:9333
	concurrency      *int //读协程并发数，默认16
	numberOfFiles    *int //每个线程写文件数量
	fileSize         *int //文件大小
	idListFile       *string //
	write            *bool //默认true
	deletePercentage *int
	read             *bool //默认true
	sequentialRead   *bool //默认随机读，false
	collection       *string //收集数据缓存
	replication      *string //拷贝类型
	cpuprofile       *string //cpu
	maxCpu           *int //
	//接口类型，接口类型执行的值
	grpcDialOption   grpc.DialOption
	//结构类型指针
	masterClient     *wdclient.MasterClient
}
//变量
var (
	b           BenchmarkOptions
	sharedBytes []byte
	isSecure    bool
)

func init() {
	cmdBenchmark.Run = runBenchmark // break init cycle
	cmdBenchmark.IsDebug = cmdBenchmark.Flag.Bool("debug", false, "verbose debug information")
	b.masters = cmdBenchmark.Flag.String("master", "localhost:9333", "SeaweedFS master location")
	b.concurrency = cmdBenchmark.Flag.Int("c", 16, "number of concurrent write or read processes")
	b.fileSize = cmdBenchmark.Flag.Int("size", 1024, "simulated file size in bytes, with random(0~63) bytes padding")
	b.numberOfFiles = cmdBenchmark.Flag.Int("n", 1024*1024, "number of files to write for each thread")
	b.idListFile = cmdBenchmark.Flag.String("list", os.TempDir()+"/benchmark_list.txt", "list of uploaded file ids")
	b.write = cmdBenchmark.Flag.Bool("write", true, "enable write")
	b.deletePercentage = cmdBenchmark.Flag.Int("deletePercent", 0, "the percent of writes that are deletes")
	b.read = cmdBenchmark.Flag.Bool("read", true, "enable read")
	b.sequentialRead = cmdBenchmark.Flag.Bool("readSequentially", false, "randomly read by ids from \"-list\" specified file")
	b.collection = cmdBenchmark.Flag.String("collection", "benchmark", "write data to this collection")
	b.replication = cmdBenchmark.Flag.String("replication", "000", "replication type")
	b.cpuprofile = cmdBenchmark.Flag.String("cpuprofile", "", "cpu profile output file")
	b.maxCpu = cmdBenchmark.Flag.Int("maxCpu", 0, "maximum number of CPUs. 0 means all available CPUs")
	sharedBytes = make([]byte, 1024)
}

var cmdBenchmark = &Command{
	UsageLine: "benchmark -server=localhost:9333 -c=10 -n=100000",
	Short:     "benchmark on writing millions of files and read out",
	Long: `benchmark on an empty SeaweedFS file system.

  Two tests during benchmark:
  1) write lots of small files to the system
  2) read the files out

  The file content is mostly zero, but no compression is done.

  You can choose to only benchmark read or write.
  During write, the list of uploaded file ids is stored in "-list" specified file.
  You can also use your own list of file ids to run read test.

  Write speed and read speed will be collected.
  The numbers are used to get a sense of the system.
  Usually your network or the hard drive is the real bottleneck.

  Another thing to watch is whether the volumes are evenly distributed
  to each volume server. Because the 7 more benchmark volumes are randomly distributed
  to servers with free slots, it's highly possible some servers have uneven amount of
  benchmark volumes. To remedy this, you can use this to grow the benchmark volumes
  before starting the benchmark command:
    http://localhost:9333/vol/grow?collection=benchmark&count=5

  After benchmarking, you can clean up the written data by deleting the benchmark collection
    http://localhost:9333/col/delete?collection=benchmark

  `,
}

var (
	//数据安全
	wait       sync.WaitGroup
	writeStats *stats
	readStats  *stats
)

//---------运行函数
func runBenchmark(cmd *Command, args []string) bool {

	weed_server.LoadConfiguration("security", false)
	b.grpcDialOption = security.LoadClientTLS(viper.Sub("grpc"), "client")

	fmt.Printf("This is SeaweedFS version %s %s %s\n", util.VERSION, runtime.GOOS, runtime.GOARCH)
	if *b.maxCpu < 1 {
		*b.maxCpu = runtime.NumCPU()
	}
	//设置最大线程数量
	runtime.GOMAXPROCS(*b.maxCpu)

	if *b.cpuprofile != "" {
		f, err := os.Create(*b.cpuprofile)
		if err != nil {
			glog.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	//b.masterClient初始化
	b.masterClient = wdclient.NewMasterClient(context.Background(), b.grpcDialOption,
		"client", strings.Split(*b.masters, ","))
	//协程测试主节点链接
	go b.masterClient.KeepConnectedToMaster()
	//主节点链接
	b.masterClient.WaitUntilConnected()

	if *b.write {
		benchWrite()
	}

	if *b.read {
		benchRead()
	}

	return true
}

func benchWrite() {
	//字符串阻塞通道
	fileIdLineChan := make(chan string)
	//bool阻塞通道
	finishChan := make(chan bool)
	//写状态
	writeStats = newStats(*b.concurrency)
	idChan := make(chan int)
	//协程写数据
	go writeFileIds(*b.idListFile, fileIdLineChan, finishChan)
	for i := 0; i < *b.concurrency; i++ {
		//控制并发写
		wait.Add(1)
		//
		go writeFiles(idChan, fileIdLineChan, &writeStats.localStats[i])
	}
	writeStats.start = time.Now()
	//numberOfFiles为每个线程写文件数量
	writeStats.total = *b.numberOfFiles
	//检查进程情况
	go writeStats.checkProgress("Writing Benchmark", finishChan)
	//numberOfFiles每个线程操作数据量
	for i := 0; i < *b.numberOfFiles; i++ {
		idChan <- i //接收数据
	}
	//关闭通道
	close(idChan)
	wait.Wait()
	writeStats.end = time.Now()
	wait.Add(2)
	finishChan <- true
	finishChan <- true
	wait.Wait()
	close(finishChan)
	writeStats.printStats()
}

func benchRead() {
	fileIdLineChan := make(chan string)
	finishChan := make(chan bool)
	readStats = newStats(*b.concurrency)
	go readFileIds(*b.idListFile, fileIdLineChan)
	readStats.start = time.Now()
	readStats.total = *b.numberOfFiles
	go readStats.checkProgress("Randomly Reading Benchmark", finishChan)
	for i := 0; i < *b.concurrency; i++ {
		wait.Add(1)
		go readFiles(fileIdLineChan, &readStats.localStats[i])
	}
	wait.Wait()
	wait.Add(1)
	finishChan <- true
	wait.Wait()
	close(finishChan)
	readStats.end = time.Now()
	readStats.printStats()
}

type delayedFile struct {
	enterTime time.Time
	fp        *operation.FilePart
}

//写文件，
// idChan每个线程的文件数量，为发送通道，
// fileIdLineChan接收通道，阻塞通道
func writeFiles(idChan <-chan int, fileIdLineChan chan<- string, s *stat) {
	//主线程并发控制
	defer wait.Done()
	//延迟删除通道（数据缓存通道），100个缓冲区，7个线程处理
	delayedDeleteChan := make(chan *delayedFile, 100)
	//写文件子线程并发控制
	var waitForDeletions sync.WaitGroup
	//使用7个线程写
	for i := 0; i < 7; i++ {
		//并发控制，加入任务
		waitForDeletions.Add(1)
		go func() {
			//写文件并发控制
			defer waitForDeletions.Done()
			//异步写文件
			for df := range delayedDeleteChan {//通道接收信息
				if df.enterTime.After(time.Now()) {
					time.Sleep(df.enterTime.Sub(time.Now()))
				}
				var jwtAuthorization security.EncodedJwt
				if isSecure {
					jwtAuthorization = operation.LookupJwt(b.masterClient.GetMaster(), df.fp.Fid)
				}
				if e := util.Delete(fmt.Sprintf("http://%s/%s", df.fp.Server, df.fp.Fid), string(jwtAuthorization)); e == nil {
					s.completed++
				} else {
					s.failed++
				}
			}
		}()
	}

	random := rand.New(rand.NewSource(time.Now().UnixNano()))
	//消费idChan通道，每个线程的文件数量
	for id := range idChan {
		start := time.Now()
		fileSize := int64(*b.fileSize + random.Intn(64))
		//
		fp := &operation.FilePart{
			Reader:   &FakeReader{id: uint64(id), size: fileSize},
			FileSize: fileSize,
			MimeType: "image/bench", // prevent gzip benchmark content
		}
		ar := &operation.VolumeAssignRequest{
			Count:       1,
			Collection:  *b.collection,
			Replication: *b.replication,
		}
		//获取结果
		if assignResult, err := operation.Assign(b.masterClient.GetMaster(), b.grpcDialOption, ar); err == nil {
			fp.Server, fp.Fid, fp.Collection = assignResult.Url, assignResult.Fid, *b.collection
			if !isSecure && assignResult.Auth != "" {
				isSecure = true
			}
			//获取更新数据
			if _, err := fp.Upload(0, b.masterClient.GetMaster(), assignResult.Auth, b.grpcDialOption); err == nil {
				if random.Intn(100) < *b.deletePercentage {
					s.total++
					//把数据发送到写数据缓冲通道，
					delayedDeleteChan <- &delayedFile{time.Now().Add(time.Second), fp}
				} else {
					//接收通道
					fileIdLineChan <- fp.Fid
				}
				s.completed++
				s.transferred += fileSize
			} else {
				s.failed++
				fmt.Printf("Failed to write with error:%v\n", err)
			}
			writeStats.addSample(time.Now().Sub(start))
			if *cmdBenchmark.IsDebug {
				fmt.Printf("writing %d file %s\n", id, fp.Fid)
			}
		} else {
			s.failed++
			println("writing file error:", err.Error())
		}
	}
	close(delayedDeleteChan)
	//阻塞等待执行完毕
	waitForDeletions.Wait()
}

//---------读文件
func readFiles(fileIdLineChan chan string, s *stat) {
	defer wait.Done()

	for fid := range fileIdLineChan {
		if len(fid) == 0 {
			continue
		}
		if fid[0] == '#' {
			continue
		}
		if *cmdBenchmark.IsDebug {
			fmt.Printf("reading file %s\n", fid)
		}
		start := time.Now()
		url, err := b.masterClient.LookupFileId(fid)
		if err != nil {
			s.failed++
			println("!!!! ", fid, " location not found!!!!!")
			continue
		}
		if bytesRead, err := util.Get(url); err == nil {
			s.completed++
			s.transferred += int64(len(bytesRead))
			readStats.addSample(time.Now().Sub(start))
		} else {
			s.failed++
			fmt.Printf("Failed to read %s error:%v\n", url, err)
		}
	}
}

func writeFileIds(fileName string, fileIdLineChan chan string, finishChan chan bool) {
	file, err := os.OpenFile(fileName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		glog.Fatalf("File to create file %s: %s\n", fileName, err)
	}
	defer file.Close()

	for {
		select {
		case <-finishChan:
			wait.Done()
			return
		case line := <-fileIdLineChan:
			file.Write([]byte(line))
			file.Write([]byte("\n"))
		}
	}
}

func readFileIds(fileName string, fileIdLineChan chan string) {
	file, err := os.Open(fileName) // For read access.
	if err != nil {
		glog.Fatalf("File to read file %s: %s\n", fileName, err)
	}
	defer file.Close()

	random := rand.New(rand.NewSource(time.Now().UnixNano()))

	r := bufio.NewReader(file)
	if *b.sequentialRead {
		for {
			if line, err := Readln(r); err == nil {
				fileIdLineChan <- string(line)
			} else {
				break
			}
		}
	} else {
		lines := make([]string, 0, readStats.total)
		for {
			if line, err := Readln(r); err == nil {
				lines = append(lines, string(line))
			} else {
				break
			}
		}
		if len(lines) > 0 {
			for i := 0; i < readStats.total; i++ {
				fileIdLineChan <- lines[random.Intn(len(lines))]
			}
		}
	}

	close(fileIdLineChan)
}
//----------------------函数定义结束------------------------------

const (
	benchResolution = 10000 //0.1 microsecond
	benchBucket     = 1000000000 / benchResolution
)

// An efficient statics collecting and rendering
//收集和提交有效的状态
type stats struct {
	data       []int
	overflow   []int
	localStats []stat
	start      time.Time
	end        time.Time
	total      int
}
//完成情况统计
type stat struct {
	completed   int
	failed      int
	total       int
	transferred int64
}

var percentages = []int{50, 66, 75, 80, 90, 95, 98, 99, 100}

func newStats(n int) *stats {
	return &stats{
		data:       make([]int, benchResolution),
		overflow:   make([]int, 0),
		localStats: make([]stat, n), //切片
	}
}

//------------stats指针开始----------------------
//加入样板
func (s *stats) addSample(d time.Duration) {
	index := int(d / benchBucket)
	if index < 0 {
		fmt.Printf("This request takes %3.1f seconds, skipping!\n", float64(index)/10000)
	} else if index < len(s.data) {
		s.data[int(d/benchBucket)]++
	} else {
		s.overflow = append(s.overflow, index)
	}
}

func (s *stats) checkProgress(testName string, finishChan chan bool) {
	fmt.Printf("\n------------ %s ----------\n", testName)
	ticker := time.Tick(time.Second)
	lastCompleted, lastTransferred, lastTime := 0, int64(0), time.Now()
	for {
		select {
		case <-finishChan:
			wait.Done()
			return
		case t := <-ticker:
			completed, transferred, taken, total := 0, int64(0), t.Sub(lastTime), s.total
			for _, localStat := range s.localStats {
				completed += localStat.completed
				transferred += localStat.transferred
				total += localStat.total
			}
			fmt.Printf("Completed %d of %d requests, %3.1f%% %3.1f/s %3.1fMB/s\n",
				completed, total, float64(completed)*100/float64(total),
				float64(completed-lastCompleted)*float64(int64(time.Second))/float64(int64(taken)),
				float64(transferred-lastTransferred)*float64(int64(time.Second))/float64(int64(taken))/float64(1024*1024),
			)
			lastCompleted, lastTransferred, lastTime = completed, transferred, t
		}
	}
}

// 打印统计信息
func (s *stats) printStats() {
	completed, failed, transferred, total := 0, 0, int64(0), s.total
	for _, localStat := range s.localStats {
		completed += localStat.completed
		failed += localStat.failed
		transferred += localStat.transferred
		total += localStat.total
	}
	timeTaken := float64(int64(s.end.Sub(s.start))) / 1000000000
	fmt.Printf("\nConcurrency Level:      %d\n", *b.concurrency)
	fmt.Printf("Time taken for tests:   %.3f seconds\n", timeTaken)
	fmt.Printf("Complete requests:      %d\n", completed)
	fmt.Printf("Failed requests:        %d\n", failed)
	fmt.Printf("Total transferred:      %d bytes\n", transferred)
	fmt.Printf("Requests per second:    %.2f [#/sec]\n", float64(completed)/timeTaken)
	fmt.Printf("Transfer rate:          %.2f [Kbytes/sec]\n", float64(transferred)/1024/timeTaken)
	n, sum := 0, 0
	min, max := 10000000, 0
	for i := 0; i < len(s.data); i++ {
		n += s.data[i]
		sum += s.data[i] * i
		if s.data[i] > 0 {
			if min > i {
				min = i
			}
			if max < i {
				max = i
			}
		}
	}
	n += len(s.overflow)
	for i := 0; i < len(s.overflow); i++ {
		sum += s.overflow[i]
		if min > s.overflow[i] {
			min = s.overflow[i]
		}
		if max < s.overflow[i] {
			max = s.overflow[i]
		}
	}
	avg := float64(sum) / float64(n)
	varianceSum := 0.0
	for i := 0; i < len(s.data); i++ {
		if s.data[i] > 0 {
			d := float64(i) - avg
			varianceSum += d * d * float64(s.data[i])
		}
	}
	for i := 0; i < len(s.overflow); i++ {
		d := float64(s.overflow[i]) - avg
		varianceSum += d * d
	}
	std := math.Sqrt(varianceSum / float64(n))
	fmt.Printf("\nConnection Times (ms)\n")
	fmt.Printf("              min      avg        max      std\n")
	fmt.Printf("Total:        %2.1f      %3.1f       %3.1f      %3.1f\n", float32(min)/10, float32(avg)/10, float32(max)/10, std/10)
	//printing percentiles
	fmt.Printf("\nPercentage of the requests served within a certain time (ms)\n")
	percentiles := make([]int, len(percentages))
	for i := 0; i < len(percentages); i++ {
		percentiles[i] = n * percentages[i] / 100
	}
	percentiles[len(percentiles)-1] = n
	percentileIndex := 0
	currentSum := 0
	for i := 0; i < len(s.data); i++ {
		currentSum += s.data[i]
		if s.data[i] > 0 && percentileIndex < len(percentiles) && currentSum >= percentiles[percentileIndex] {
			fmt.Printf("  %3d%%    %5.1f ms\n", percentages[percentileIndex], float32(i)/10.0)
			percentileIndex++
			for percentileIndex < len(percentiles) && currentSum >= percentiles[percentileIndex] {
				percentileIndex++
			}
		}
	}
	sort.Ints(s.overflow)
	for i := 0; i < len(s.overflow); i++ {
		currentSum++
		if percentileIndex < len(percentiles) && currentSum >= percentiles[percentileIndex] {
			fmt.Printf("  %3d%%    %5.1f ms\n", percentages[percentileIndex], float32(s.overflow[i])/10.0)
			percentileIndex++
			for percentileIndex < len(percentiles) && currentSum >= percentiles[percentileIndex] {
				percentileIndex++
			}
		}
	}
}

//-------------stats指针方法结束---------------------

// a fake reader to generate content to upload
type FakeReader struct {
	id   uint64 // an id number
	size int64  // max bytes
}

func (l *FakeReader) Read(p []byte) (n int, err error) {
	if l.size <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > l.size {
		n = int(l.size)
	} else {
		n = len(p)
	}
	if n >= 8 {
		for i := 0; i < 8; i++ {
			p[i] = byte(l.id >> uint(i*8))
		}
	}
	l.size -= int64(n)
	return
}

func (l *FakeReader) WriteTo(w io.Writer) (n int64, err error) {
	size := int(l.size)
	bufferSize := len(sharedBytes)
	for size > 0 {
		tempBuffer := sharedBytes
		if size < bufferSize {
			tempBuffer = sharedBytes[0:size]
		}
		count, e := w.Write(tempBuffer)
		if e != nil {
			return int64(size), e
		}
		size -= count
	}
	return l.size, nil
}

func Readln(r *bufio.Reader) ([]byte, error) {
	var (
		isPrefix = true
		err      error
		line, ln []byte
	)
	for isPrefix && err == nil {
		//读入一行
		line, isPrefix, err = r.ReadLine()
		ln = append(ln, line...)
	}
	return ln, err
}