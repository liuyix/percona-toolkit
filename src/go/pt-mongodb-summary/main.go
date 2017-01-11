package main

import (
	"fmt"
	"html/template"
	"os"
	"strings"
	"time"

	"github.com/howeyc/gopass"
	"github.com/pborman/getopt"
	"github.com/percona/percona-toolkit/src/go/lib/config"
	"github.com/percona/percona-toolkit/src/go/lib/util"
	"github.com/percona/percona-toolkit/src/go/lib/versioncheck"
	"github.com/percona/percona-toolkit/src/go/mongolib/proto"
	"github.com/percona/percona-toolkit/src/go/pt-mongodb-summary/oplog"
	"github.com/percona/percona-toolkit/src/go/pt-mongodb-summary/templates"
	"github.com/percona/pmgo"
	"github.com/pkg/errors"
	"github.com/shirou/gopsutil/process"
	log "github.com/sirupsen/logrus"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

const (
	TOOLNAME = "pt-mongodb-summary"
)

var (
	Version   string = "2.2.19"
	Build     string = "01-01-1980"
	GoVersion string = "1.8"
)

type TimedStats struct {
	Min   int64
	Max   int64
	Total int64
	Avg   int64
}

type opCounters struct {
	Insert     TimedStats
	Query      TimedStats
	Update     TimedStats
	Delete     TimedStats
	GetMore    TimedStats
	Command    TimedStats
	SampleRate time.Duration
}
type hostInfo struct {
	ThisHostID        int
	Hostname          string
	HostOsType        string
	HostSystemCPUArch string
	HostDatabases     int
	HostCollections   int
	DBPath            string

	ProcPath         string
	ProcUserName     string
	ProcCreateTime   time.Time
	ProcProcessCount int

	// Server Status
	ProcessName    string
	ReplicasetName string
	Version        string
	NodeType       string
}

type procInfo struct {
	CreateTime time.Time
	Path       string
	UserName   string
	Error      error
}

type security struct {
	Users int
	Roles int
	Auth  string
	SSL   string
}

type databases struct {
	Databases []struct {
		Name       string           `bson:"name"`
		SizeOnDisk int64            `bson:"sizeOnDisk"`
		Empty      bool             `bson:"empty"`
		Shards     map[string]int64 `bson:"shards"`
	} `bson:"databases"`
	TotalSize   int64 `bson:"totalSize"`
	TotalSizeMb int64 `bson:"totalSizeMb"`
	OK          bool  `bson:"ok"`
}

type clusterwideInfo struct {
	TotalDBsCount           int
	TotalCollectionsCount   int
	ShardedColsCount        int
	UnshardedColsCount      int
	ShardedDataSize         int64 // bytes
	ShardedDataSizeScaled   float64
	ShardedDataSizeScale    string
	UnshardedDataSize       int64 // bytes
	UnshardedDataSizeScaled float64
	UnshardedDataSizeScale  string
}

type options struct {
	Host           string
	User           string
	Password       string
	AuthDB         string
	LogLevel       string
	Version        bool
	NoVersionCheck bool
}

func main() {

	opts := options{Host: "localhost:27017", LogLevel: "error"}
	help := getopt.BoolLong("help", '?', "Show help")
	getopt.BoolVarLong(&opts.Version, "version", 'v', "", "Show version & exit")
	getopt.BoolVarLong(&opts.NoVersionCheck, "no-version-check", 'c', "", "Don't check for updates")

	getopt.StringVarLong(&opts.User, "user", 'u', "", "User name")
	getopt.StringVarLong(&opts.Password, "password", 'p', "", "Password").SetOptional()
	getopt.StringVarLong(&opts.AuthDB, "authenticationDatabase", 'a', "admin", "Database used to establish credentials and privileges with a MongoDB server")
	getopt.StringVarLong(&opts.LogLevel, "log-level", 'l', "error", "Log level:, panic, fatal, error, warn, info, debug")
	getopt.SetParameters("host[:port]")

	getopt.Parse()
	if *help {
		getopt.Usage()
		return
	}

	logLevel, err := log.ParseLevel(opts.LogLevel)
	if err != nil {
		fmt.Printf("cannot set log level: %s", err.Error())
	}

	log.SetLevel(logLevel)

	args := getopt.Args() // positional arg
	if len(args) > 0 {
		opts.Host = args[0]
	}

	if opts.Version {
		fmt.Println("pt-mongodb-summary")
		fmt.Printf("Version %s\n", Version)
		fmt.Printf("Build: %s using %s\n", Build, GoVersion)
		return
	}

	conf := config.DefaultConfig(TOOLNAME)
	if !conf.GetBool("no-version-check") && !opts.NoVersionCheck {
		advice, err := versioncheck.CheckUpdates(TOOLNAME, Version)
		if err != nil {
			log.Infof("cannot check version updates: %s", err.Error())
		} else {
			if advice != "" {
				log.Infof(advice)
			}
		}
	}

	if getopt.IsSet("password") && opts.Password == "" {
		print("Password: ")
		pass, err := gopass.GetPasswd()
		if err != nil {
			fmt.Println(err)
			os.Exit(2)
		}
		opts.Password = string(pass)
	}

	di := &mgo.DialInfo{
		Username: opts.User,
		Password: opts.Password,
		Addrs:    []string{opts.Host},
		FailFast: true,
		Source:   opts.AuthDB,
	}

	log.Debugf("Connecting to the db using:\n%+v", di)
	dialer := pmgo.NewDialer()

	hostnames, err := getHostnames(dialer, di)

	session, err := dialer.DialWithInfo(di)
	if err != nil {
		log.Errorf("cannot connect to the db: %s", err)
		os.Exit(1)
	}
	defer session.Close()

	if replicaMembers, err := GetReplicasetMembers(dialer, hostnames, di); err != nil {
		log.Printf("[Error] cannot get replicaset members: %v\n", err)
	} else {
		t := template.Must(template.New("replicas").Parse(templates.Replicas))
		t.Execute(os.Stdout, replicaMembers)
	}

	//
	if hostInfo, err := GetHostinfo(session); err != nil {
		log.Printf("[Error] cannot get host info: %v\n", err)
	} else {
		t := template.Must(template.New("hosttemplateData").Parse(templates.HostInfo))
		t.Execute(os.Stdout, hostInfo)
	}

	var sampleCount int64 = 5
	var sampleRate time.Duration = 1 * time.Second // in seconds
	if rops, err := GetOpCountersStats(session, sampleCount, sampleRate); err != nil {
		log.Printf("[Error] cannot get Opcounters stats: %v\n", err)
	} else {
		t := template.Must(template.New("runningOps").Parse(templates.RunningOps))
		t.Execute(os.Stdout, rops)
	}

	if security, err := GetSecuritySettings(session); err != nil {
		log.Printf("[Error] cannot get security settings: %v\n", err)
	} else {
		t := template.Must(template.New("ssl").Parse(templates.Security))
		t.Execute(os.Stdout, security)
	}

	if oplogInfo, err := oplog.GetOplogInfo(hostnames, di); err != nil {
		log.Printf("[Error] cannot get Oplog info: %v\n", err)
	} else {
		if len(oplogInfo) > 0 {
			t := template.Must(template.New("oplogInfo").Parse(templates.Oplog))
			t.Execute(os.Stdout, oplogInfo[0])
		}
	}

	if cwi, err := GetClusterwideInfo(session); err != nil {
		log.Printf("[Error] cannot get cluster wide info: %v\n", err)
	} else {
		t := template.Must(template.New("clusterwide").Parse(templates.Clusterwide))
		t.Execute(os.Stdout, cwi)
	}

	if bs, err := GetBalancerStats(session); err != nil {
		log.Printf("[Error] cannot get balancer stats: %v\n", err)
	} else {
		t := template.Must(template.New("balancer").Parse(templates.BalancerStats))
		t.Execute(os.Stdout, bs)
	}

}

func GetHostinfo(session pmgo.SessionManager) (*hostInfo, error) {

	hi := proto.HostInfo{}
	if err := session.Run(bson.M{"hostInfo": 1}, &hi); err != nil {
		return nil, errors.Wrap(err, "GetHostInfo.hostInfo")
	}

	cmdOpts := proto.CommandLineOptions{}
	err := session.DB("admin").Run(bson.D{{"getCmdLineOpts", 1}, {"recordStats", 1}}, &cmdOpts)
	if err != nil {
		return nil, errors.Wrap(err, "cannot get command line options")
	}

	ss := proto.ServerStatus{}
	if err := session.DB("admin").Run(bson.D{{"serverStatus", 1}, {"recordStats", 1}}, &ss); err != nil {
		return nil, errors.Wrap(err, "GetHostInfo.serverStatus")
	}

	pi := procInfo{}
	if err := getProcInfo(int32(ss.Pid), &pi); err != nil {
		pi.Error = err
	}

	nodeType, _ := getNodeType(session)

	i := &hostInfo{
		Hostname:          hi.System.Hostname,
		HostOsType:        hi.Os.Type,
		HostSystemCPUArch: hi.System.CpuArch,
		HostDatabases:     hi.DatabasesCount,
		HostCollections:   hi.CollectionsCount,
		DBPath:            "", // Sets default. It will be overriden later if necessary

		ProcessName: ss.Process,
		Version:     ss.Version,
		NodeType:    nodeType,

		ProcPath:       pi.Path,
		ProcUserName:   pi.UserName,
		ProcCreateTime: pi.CreateTime,
	}
	if ss.Repl != nil {
		i.ReplicasetName = ss.Repl.SetName
	}

	if cmdOpts.Parsed.Storage.DbPath != "" {
		i.DBPath = cmdOpts.Parsed.Storage.DbPath
	}

	return i, nil
}

func getHostnames(dialer pmgo.Dialer, di *mgo.DialInfo) ([]string, error) {

	session, err := dialer.DialWithInfo(di)
	if err != nil {
		return nil, err
	}
	defer session.Close()

	shardsInfo := &proto.ShardsInfo{}
	log.Debugf("Running 'listShards' command")
	err = session.Run("listShards", shardsInfo)
	if err != nil {
		return nil, errors.Wrap(err, "cannot list shards")
	}

	log.Debugf("listShards raw response: %+v", util.Pretty(shardsInfo))

	hostnames := []string{di.Addrs[0]}
	if shardsInfo != nil {
		for _, shardInfo := range shardsInfo.Shards {
			m := strings.Split(shardInfo.Host, "/")
			h := strings.Split(m[1], ",")
			hostnames = append(hostnames, h[0])
		}
	}
	return hostnames, nil
}

func GetClusterwideInfo(session pmgo.SessionManager) (*clusterwideInfo, error) {
	var databases databases

	err := session.Run(bson.M{"listDatabases": 1}, &databases)
	if err != nil {
		return nil, errors.Wrap(err, "GetClusterwideInfo.listDatabases ")
	}

	cwi := &clusterwideInfo{
		TotalDBsCount: len(databases.Databases),
	}

	for _, db := range databases.Databases {
		collections, err := session.DB(db.Name).CollectionNames()
		if err != nil {
			continue
		}
		cwi.TotalCollectionsCount += len(collections)
		for _, collName := range collections {
			var collStats proto.CollStats
			err := session.DB(db.Name).Run(bson.M{"collStats": collName}, &collStats)
			if err != nil {
				continue
			}

			if collStats.Sharded {
				cwi.ShardedDataSize += collStats.Size
				cwi.ShardedColsCount++
				continue
			}

			cwi.UnshardedDataSize += collStats.Size
			cwi.UnshardedColsCount++
		}

	}

	cwi.UnshardedColsCount = cwi.TotalCollectionsCount - cwi.ShardedColsCount
	cwi.ShardedDataSizeScaled, cwi.ShardedDataSizeScale = sizeAndUnit(cwi.ShardedDataSize)
	cwi.UnshardedDataSizeScaled, cwi.UnshardedDataSizeScale = sizeAndUnit(cwi.UnshardedDataSize)

	return cwi, nil
}

func sizeAndUnit(size int64) (float64, string) {
	unit := []string{"bytes", "KB", "MB", "GB", "TB"}
	idx := 0
	newSize := float64(size)
	for newSize > 1024 {
		newSize /= 1024
		idx++
	}
	newSize = float64(int64(newSize*100)) / 100
	return newSize, unit[idx]
}

func GetReplicasetMembers(dialer pmgo.Dialer, hostnames []string, di *mgo.DialInfo) ([]proto.Members, error) {
	replicaMembers := []proto.Members{}

	for _, hostname := range hostnames {
		di.Addrs = []string{hostname}
		session, err := dialer.DialWithInfo(di)
		if err != nil {
			return nil, errors.Wrapf(err, "getReplicasetMembers. cannot connect to %s", hostname)
		}
		defer session.Close()

		rss := proto.ReplicaSetStatus{}
		err = session.Run(bson.M{"replSetGetStatus": 1}, &rss)
		if err != nil {
			continue // If a host is a mongos we cannot get info but is not a real error
		}
		for _, m := range rss.Members {
			m.Set = rss.Set
			replicaMembers = append(replicaMembers, m)
		}
	}

	return replicaMembers, nil
}

func GetSecuritySettings(session pmgo.SessionManager) (*security, error) {
	s := security{
		Auth: "disabled",
		SSL:  "disabled",
	}

	cmdOpts := proto.CommandLineOptions{}
	err := session.DB("admin").Run(bson.D{{"getCmdLineOpts", 1}, {"recordStats", 1}}, &cmdOpts)
	if err != nil {
		return nil, errors.Wrap(err, "cannot get command line options")
	}

	if cmdOpts.Security.Authorization != "" || cmdOpts.Security.KeyFile != "" {
		s.Auth = "enabled"
	}
	if cmdOpts.Parsed.Net.SSL.Mode != "" && cmdOpts.Parsed.Net.SSL.Mode != "disabled" {
		s.SSL = cmdOpts.Parsed.Net.SSL.Mode
	}

	s.Users, err = session.DB("admin").C("system.users").Count()
	if err != nil {
		return nil, errors.Wrap(err, "cannot get users count")
	}

	s.Roles, err = session.DB("admin").C("system.roles").Count()
	if err != nil {
		return nil, errors.Wrap(err, "cannot get roles count")
	}

	return &s, nil
}

func getNodeType(session pmgo.SessionManager) (string, error) {
	md := proto.MasterDoc{}
	err := session.Run("isMaster", &md)
	if err != nil {
		return "", err
	}

	if md.SetName != nil || md.Hosts != nil {
		return "replset", nil
	} else if md.Msg == "isdbgrid" {
		// isdbgrid is always the msg value when calling isMaster on a mongos
		// see http://docs.mongodb.org/manual/core/sharded-cluster-query-router/
		return "mongos", nil
	}
	return "mongod", nil
}

func GetOpCountersStats(session pmgo.SessionManager, count int64, sleep time.Duration) (*opCounters, error) {
	oc := &opCounters{}
	prevOpCount := &opCounters{}
	ss := proto.ServerStatus{}
	delta := proto.ServerStatus{
		Opcounters: &proto.OpcountStats{},
	}

	ticker := time.NewTicker(sleep)
	for i := int64(0); i < count+1; i++ {
		<-ticker.C
		err := session.DB("admin").Run(bson.D{{"serverStatus", 1}, {"recordStats", 1}}, &ss)
		if err != nil {
			panic(err)
		}

		if i == 0 {
			prevOpCount.Command.Total = ss.Opcounters.Command
			prevOpCount.Delete.Total = ss.Opcounters.Delete
			prevOpCount.GetMore.Total = ss.Opcounters.GetMore
			prevOpCount.Insert.Total = ss.Opcounters.Insert
			prevOpCount.Query.Total = ss.Opcounters.Query
			prevOpCount.Update.Total = ss.Opcounters.Update
			continue
		}

		delta.Opcounters.Command = ss.Opcounters.Command - prevOpCount.Command.Total
		delta.Opcounters.Delete = ss.Opcounters.Delete - prevOpCount.Delete.Total
		delta.Opcounters.GetMore = ss.Opcounters.GetMore - prevOpCount.GetMore.Total
		delta.Opcounters.Insert = ss.Opcounters.Insert - prevOpCount.Insert.Total
		delta.Opcounters.Query = ss.Opcounters.Query - prevOpCount.Query.Total
		delta.Opcounters.Update = ss.Opcounters.Update - prevOpCount.Update.Total

		// Be careful. This cannot be item[0] because we need: value - prev_value
		// and at pos 0 there is no prev value
		if i == 1 {
			oc.Command.Max = delta.Opcounters.Command
			oc.Command.Min = delta.Opcounters.Command

			oc.Delete.Max = delta.Opcounters.Delete
			oc.Delete.Min = delta.Opcounters.Delete

			oc.GetMore.Max = delta.Opcounters.GetMore
			oc.GetMore.Min = delta.Opcounters.GetMore

			oc.Insert.Max = delta.Opcounters.Insert
			oc.Insert.Min = delta.Opcounters.Insert

			oc.Query.Max = delta.Opcounters.Query
			oc.Query.Min = delta.Opcounters.Query

			oc.Update.Max = delta.Opcounters.Update
			oc.Update.Min = delta.Opcounters.Update
		}

		// Insert --------------------------------------
		if delta.Opcounters.Insert > oc.Insert.Max {
			oc.Insert.Max = delta.Opcounters.Insert
		}
		if delta.Opcounters.Insert < oc.Insert.Min {
			oc.Insert.Min = delta.Opcounters.Insert
		}
		oc.Insert.Total += delta.Opcounters.Insert

		// Query ---------------------------------------
		if delta.Opcounters.Query > oc.Query.Max {
			oc.Query.Max = delta.Opcounters.Query
		}
		if delta.Opcounters.Query < oc.Query.Min {
			oc.Query.Min = delta.Opcounters.Query
		}
		oc.Query.Total += delta.Opcounters.Query

		// Command -------------------------------------
		if delta.Opcounters.Command > oc.Command.Max {
			oc.Command.Max = delta.Opcounters.Command
		}
		if delta.Opcounters.Command < oc.Command.Min {
			oc.Command.Min = delta.Opcounters.Command
		}
		oc.Command.Total += delta.Opcounters.Command

		// Update --------------------------------------
		if delta.Opcounters.Update > oc.Update.Max {
			oc.Update.Max = delta.Opcounters.Update
		}
		if delta.Opcounters.Update < oc.Update.Min {
			oc.Update.Min = delta.Opcounters.Update
		}
		oc.Update.Total += delta.Opcounters.Update

		// Delete --------------------------------------
		if delta.Opcounters.Delete > oc.Delete.Max {
			oc.Delete.Max = delta.Opcounters.Delete
		}
		if delta.Opcounters.Delete < oc.Delete.Min {
			oc.Delete.Min = delta.Opcounters.Delete
		}
		oc.Delete.Total += delta.Opcounters.Delete

		// GetMore -------------------------------------
		if delta.Opcounters.GetMore > oc.GetMore.Max {
			oc.GetMore.Max = delta.Opcounters.GetMore
		}
		if delta.Opcounters.GetMore < oc.GetMore.Min {
			oc.GetMore.Min = delta.Opcounters.GetMore
		}
		oc.GetMore.Total += delta.Opcounters.GetMore

		prevOpCount.Insert.Total = ss.Opcounters.Insert
		prevOpCount.Query.Total = ss.Opcounters.Query
		prevOpCount.Command.Total = ss.Opcounters.Command
		prevOpCount.Update.Total = ss.Opcounters.Update
		prevOpCount.Delete.Total = ss.Opcounters.Delete
		prevOpCount.GetMore.Total = ss.Opcounters.GetMore

	}
	ticker.Stop()

	oc.Insert.Avg = oc.Insert.Total
	oc.Query.Avg = oc.Query.Total
	oc.Update.Avg = oc.Update.Total
	oc.Delete.Avg = oc.Delete.Total
	oc.GetMore.Avg = oc.GetMore.Total
	oc.Command.Avg = oc.Command.Total
	//
	oc.SampleRate = time.Duration(count) * sleep

	return oc, nil
}

func getProcInfo(pid int32, templateData *procInfo) error {
	//proc, err := process.NewProcess(templateData.ServerStatus.Pid)
	proc, err := process.NewProcess(pid)
	if err != nil {
		return fmt.Errorf("cannot get process %d\n", pid)
	}
	ct, err := proc.CreateTime()
	if err != nil {
		return err
	}

	templateData.CreateTime = time.Unix(ct/1000, 0)
	templateData.Path, err = proc.Exe()
	if err != nil {
		return err
	}

	templateData.UserName, err = proc.Username()
	if err != nil {
		return err
	}
	return nil
}

func getDbsAndCollectionsCount(hostnames []string) (int, int, error) {
	dbnames := make(map[string]bool)
	colnames := make(map[string]bool)

	for _, hostname := range hostnames {
		session, err := mgo.Dial(hostname)
		if err != nil {
			continue
		}
		dbs, err := session.DatabaseNames()
		if err != nil {
			continue
		}

		for _, dbname := range dbs {
			dbnames[dbname] = true
			cols, err := session.DB(dbname).CollectionNames()
			if err != nil {
				continue
			}
			for _, colname := range cols {
				colnames[dbname+"."+colname] = true
			}
		}
	}

	return len(dbnames), len(colnames), nil
}

func GetBalancerStats(session pmgo.SessionManager) (*proto.BalancerStats, error) {

	scs, err := GetShardingChangelogStatus(session)
	if err != nil {
		return nil, err
	}

	s := &proto.BalancerStats{}

	for _, item := range *scs.Items {
		event := item.Id.Event
		note := item.Id.Note
		count := item.Count
		switch event {
		case "moveChunk.to", "moveChunk.from", "moveChunk.commit":
			if note == "success" || note == "" {
				s.Success += int64(count)
			} else {
				s.Failed += int64(count)
			}
		case "split", "multi-split":
			s.Splits += int64(count)
		case "dropCollection", "dropCollection.start", "dropDatabase", "dropDatabase.start":
			s.Drops++
		}
	}

	return s, nil
}

func GetShardingChangelogStatus(session pmgo.SessionManager) (*proto.ShardingChangelogStats, error) {
	var qresults []proto.ShardingChangelogSummary
	coll := session.DB("config").C("changelog")
	match := bson.M{"time": bson.M{"$gt": time.Now().Add(-240 * time.Hour)}}
	group := bson.M{"_id": bson.M{"event": "$what", "note": "$details.note"}, "count": bson.M{"$sum": 1}}

	err := coll.Pipe([]bson.M{{"$match": match}, {"$group": group}}).All(&qresults)
	if err != nil {
		return nil, errors.Wrap(err, "GetShardingChangelogStatus.changelog.find")
	}

	return &proto.ShardingChangelogStats{
		Items: &qresults,
	}, nil
}
