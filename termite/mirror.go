package termite

import (
	"net"
	"os"
	"log"
	"rpc"
	"sync"
)

// State associated with one master.
type Mirror struct {
	daemon         *WorkerDaemon
	fileServer     *rpc.Client
	fileServerConn net.Conn
	rpcConn        net.Conn
	rpcFs          *RpcFs
	writableRoot   string

	// key in WorkerDaemon's map.
	key string

	Waiting              int
	maxJobCount          int
	fuseFileSystemsMutex sync.Mutex
	unusedFileSystems    []*workerFuseFs

	// Map value is the command running.
	workingFileSystems map[*workerFuseFs]string
	shuttingDown       bool
	cond               *sync.Cond
}

func NewMirror(daemon *WorkerDaemon, rpcConn, revConn net.Conn) *Mirror {
	log.Println("Mirror for", rpcConn, revConn)

	mirror := &Mirror{
		fileServerConn:     revConn,
		rpcConn:            rpcConn,
		fileServer:         rpc.NewClient(revConn),
		daemon:             daemon,
		workingFileSystems: make(map[*workerFuseFs]string),
	}
	mirror.rpcFs = NewRpcFs(mirror.fileServer, daemon.contentCache)
	mirror.rpcFs.localRoots = []string{"/lib", "/usr"}
	mirror.cond = sync.NewCond(&mirror.fuseFileSystemsMutex)

	go mirror.serveRpc()
	return mirror
}

func (me *Mirror) discardFuse(wfs *workerFuseFs) {
	wfs.Stop()

	me.fuseFileSystemsMutex.Lock()
	defer me.fuseFileSystemsMutex.Unlock()
	me.workingFileSystems[wfs] = "", false
	me.cond.Broadcast()
}

func (me *Mirror) serveRpc() {
	server := rpc.NewServer()
	server.Register(me)
	server.ServeConn(me.rpcConn)
	me.Shutdown()
}

func (me *Mirror) Shutdown() {
	me.fuseFileSystemsMutex.Lock()
	defer me.fuseFileSystemsMutex.Unlock()
	if me.shuttingDown {
		return
	}
	me.shuttingDown = true
	me.fileServer.Close()
	me.fileServerConn.Close()
	for _, fs := range me.unusedFileSystems {
		fs.Stop()
	}
	me.unusedFileSystems = nil

	for len(me.workingFileSystems) > 0 {
		me.maxJobCount = len(me.workingFileSystems)
		me.cond.Wait()
	}
	me.rpcConn.Close()

	go me.daemon.DropMirror(me)
}

func (me *Mirror) getWorkerFuseFs(name string) (f *workerFuseFs, err os.Error) {
	me.fuseFileSystemsMutex.Lock()
	defer me.fuseFileSystemsMutex.Unlock()

	me.Waiting++
	for len(me.workingFileSystems) >= me.maxJobCount {
		me.cond.Wait()
	}
	me.Waiting--
	if me.shuttingDown {
		return nil, os.NewError("shutting down")
	}

	l := len(me.unusedFileSystems)
	if l > 0 {
		f = me.unusedFileSystems[l-1]
		me.unusedFileSystems = me.unusedFileSystems[:l-1]
	}
	if f == nil {
		f, err = me.newWorkerFuseFs()
	}
	me.workingFileSystems[f] = name
	return f, err
}

func (me *Mirror) Update(req *UpdateRequest, rep *UpdateResponse) os.Error {
	me.updateFiles(req.Files, nil)
	return nil
}

func (me *Mirror) updateFiles(attrs []*FileAttr, origin *workerFuseFs) {
	me.rpcFs.updateFiles(attrs)

	me.fuseFileSystemsMutex.Lock()
	defer me.fuseFileSystemsMutex.Unlock()

	for _, fs := range me.unusedFileSystems {
		fs.update(attrs, origin)
	}
	for fs, _ := range me.workingFileSystems {
		fs.update(attrs, origin)
	}
}

func (me *Mirror) fetchFiles(files []*FileAttr) {
	for _, f := range files {
		if f.Hash != "" {
			me.rpcFs.FetchHash(f.FileInfo.Size, f.Hash)
		}
	}
}

func (me *Mirror) Run(req *WorkRequest, rep *WorkResponse) os.Error {
	// Don't run me.updateFiles() as we don't want to issue
	// unneeded cache invalidations.
	me.rpcFs.updateFiles(req.Prefetch)
	go me.fetchFiles(req.Prefetch)
	task, err := me.newWorkerTask(req, rep)
	if err != nil {
		return err
	}

	err = task.Run()
	if err != nil {
		log.Println("task.Run:", err)
		return err
	}

	rep.LastTime = 0
	summary := *rep
	summary.Stdout = HumanTrim(summary.Stdout, 1024)
	summary.Stderr = HumanTrim(summary.Stderr, 1024)
	
	log.Println("sending back", summary)
	return nil
}

const _DELETIONS = "DELETIONS"

func (me *Mirror) newWorkerFuseFs() (*workerFuseFs, os.Error) {
	return newWorkerFuseFs(me.daemon.tmpDir, me.rpcFs, me.writableRoot, me.daemon.Nobody)
}

func (me *Mirror) newWorkerTask(req *WorkRequest, rep *WorkResponse) (*WorkerTask, os.Error) {
	var stdin net.Conn
	if req.StdinId != "" {
		stdin = me.daemon.pending.WaitConnection(req.StdinId)
	}
	task := &WorkerTask{
		WorkRequest:  req,
		WorkResponse: rep,
		stdinConn:    stdin,
		mirror:       me,
	}
	return task, nil
}

func (me *Mirror) FileContent(req *ContentRequest, rep *ContentResponse) os.Error {
	return me.daemon.contentServer.FileContent(req, rep)
}
