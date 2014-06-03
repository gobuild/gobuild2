package slave

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/codegangsta/cli"
	"github.com/codeskyblue/go-sh"
	"github.com/gobuild/gobuild2/models"
	"github.com/gobuild/gobuild2/pkg/xrpc"
	"github.com/gobuild/log"
)

var (
	TMPDIR     = "./tmp"
	PROGRAM, _ = filepath.Abs(os.Args[0])
	SELFDIR    = filepath.Dir(PROGRAM)
	GOPM       = filepath.Join(SELFDIR, "bin/gopm")
	HOSTNAME   = "localhost"
	HOSTINFO   = &xrpc.HostInfo{Os: runtime.GOOS, Arch: runtime.GOARCH, Host: HOSTNAME}
)

func checkError(err error) {
	if err != nil {
		log.Errorf("err: %v", err)
	}
}

type NTMsg struct {
	Status string
	Output string
	Extra  string
}

func GoInterval(dur time.Duration, f func()) chan bool {
	done := make(chan bool)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(dur):
				f()
			}
		}
	}()
	return done
}

func work(m *xrpc.Mission) (err error) {
	notify := func(status string, output string, extra ...string) {
		mstatus := &xrpc.MissionStatus{Mid: m.Mid, Status: status,
			Output: output,
			Extra:  strings.Join(extra, ""),
		}
		ok := false
		err := xrpc.Call("UpdateMissionStatus", mstatus, &ok)
		checkError(err)
	}
	defer func() {
		fmt.Println("DONE", err)
		if err != nil {
			notify(models.ST_ERROR, err.Error())
		}
	}()
	// prepare shell session
	sess := sh.NewSession()
	buffer := bytes.NewBuffer(nil)
	sess.Stdout = io.MultiWriter(buffer, os.Stdout)
	sess.Stderr = io.MultiWriter(buffer, os.Stderr)
	sess.ShowCMD = true
	gopath, err := ioutil.TempDir(TMPDIR, time.Now().Format("200601021504-"))
	if err != nil {
		log.Errorf("create gopath error: %v", err)
		return
	}
	// fmt.Println(gopath)
	// return
	// var gopath, _ = filepath.Abs(TMPDIR)
	if !sh.Test("dir", gopath) {
		os.MkdirAll(gopath, 0755)
	}
	defer os.RemoveAll(gopath)
	sess.SetEnv("GOPATH", gopath)
	sess.SetEnv("CGO_ENABLE", "0")
	if m.CgoEnable {
		sess.SetEnv("CGO_ENABLE", "1")
	}
	sess.SetEnv("GOOS", m.Os)
	sess.SetEnv("GOARCH", m.Arch)
	sess.SetTimeout(time.Minute * 10) // timeout in 10minutes

	var repoName = m.Repo
	var srcPath = filepath.Join(gopath, "src", repoName)

	getsrc := func() (err error) {
		var params []interface{}
		params = append(params, "get", "-v", "-g") // todo: add -d when gopm released
		if m.Sha != "" {
			params = append(params, repoName+"@commit:"+m.Sha)
		} else {
			params = append(params, repoName+"@branch:"+m.Branch)
		}
		params = append(params, sh.Dir(gopath))
		if err = sess.Command(GOPM, params...).Run(); err != nil {
			return
		}
		return nil
	}

	build := func() (err error) {
		err = sess.Command("go", "get", "-v", sh.Dir(srcPath)).Run()
		if err != nil {
			return
		}
		return sess.Command("go", "build", "-v", sh.Dir(srcPath)).Run()
	}
	newNotify := func(status string, buf *bytes.Buffer) chan bool {
		return GoInterval(time.Second*2, func() {
			notify(status, string(buf.Bytes()))
		})
	}

	notify(models.ST_RETRIVING, "start get source")
	var done chan bool
	done = newNotify(models.ST_RETRIVING, buffer)
	err = getsrc()
	done <- true
	notify(models.ST_RETRIVING, string(buffer.Bytes()))
	if err != nil {
		log.Errorf("getsource err: %v", err)
		return
	}
	buffer.Reset()

	var outFile = filepath.Base(m.UpKey)
	var outFullPath = filepath.Join(srcPath, outFile)

	done = newNotify(models.ST_BUILDING, buffer)

	// err = sess.Command(GOPM, "build", "-u", "-v", sh.Dir(srcPath)).Run()
	err = build()
	done <- true
	notify(models.ST_BUILDING, string(buffer.Bytes()))
	if err != nil {
		log.Errorf("build error: %v", err)
		return
	}
	buffer.Reset()

	// write extra pkginfo
	pkginfo := "pkginfo.json"
	ioutil.WriteFile(filepath.Join(srcPath, pkginfo), m.PkgInfo, 0644)

	err = sess.Command(PROGRAM, "pack",
		"--nobuild", "-a", pkginfo, "-o", outFile, sh.Dir(srcPath)).Run()
	notify(models.ST_PACKING, string(buffer.Bytes()))
	if err != nil {
		log.Error(err)
		return
	}

	var cdnPath = m.UpKey
	notify(models.ST_PUBLISHING, cdnPath)
	log.Infof("cdn path: %s", cdnPath)
	q := &Qiniu{m.UpToken, m.UpKey, m.Bulket} // uptoken, key}
	var pubAddr string
	if pubAddr, err = q.Upload(outFullPath); err != nil {
		checkError(err)
		return
	}

	log.Debugf("publish %s to %s", outFile, pubAddr)
	notify(models.ST_DONE, pubAddr)
	return nil
}

func init() {
	var err error
	HOSTNAME, err = os.Hostname()
	if err != nil {
		log.Fatalf("hostname retrive err: %v", err)
	}
}

var IsPrivateUpload bool //todo

func prepare() (err error) {
	TMPDIR, err = filepath.Abs(TMPDIR)
	if err != nil {
		log.Errorf("tmpdir to abspath err: %v", err)
		return
	}
	if !sh.Test("dir", TMPDIR) {
		os.MkdirAll(TMPDIR, 0755)
	}
	if err = setUp(); err != nil {
		log.Fatalf("setUp environment error:%v", err)
	}
	startWork()
	return nil
}

func Action(c *cli.Context) {
	fmt.Println("this is slave daemon")
	webaddr := c.String("webaddr")
	xrpc.DefaultWebAddress = webaddr

	if err := prepare(); err != nil {
		log.Fatalf("slave prepare err: %v", err)
	}
	for {
		mission := &xrpc.Mission{}
		if err := xrpc.Call("GetMission", HOSTINFO, mission); err != nil {
			log.Errorf("get mission failed: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}

		if mission.Idle != 0 {
			fmt.Print(".")
			time.Sleep(mission.Idle)
			continue
		}
		log.Infof("new mission from xrpc: %v", mission)
		missionQueue <- mission
	}
}
