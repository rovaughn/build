package main

import (
	"flag"
	"fmt"
	"github.com/fsnotify/fsnotify"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
	"gopkg.in/yaml.v2"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var recipes map[string][]string

type command interface {
	run(dir string) error
}

type server interface {
	kill()
	listSources() []string
	wait() chan error
}

type serveCommand interface {
	serve(dir string, sources []string) (server, error)
}

type runCommand interface {
	run(dir string) error
}

type includeCommand struct {
	artifacts []string
	into      string
	copy      bool
}

func (c *includeCommand) run(dir string) error {
	for _, artifact := range c.artifacts {
		artifactPath, err := filepath.Abs(artifact)
		if err != nil {
			return err
		}

		if c.copy {
			fin, err := os.Open(artifactPath)
			if err != nil {
				return err
			}
			defer fin.Close()

			fout, err := os.Create(filepath.Join(dir, c.into, artifact))
			if err != nil {
				return err
			}
			defer fout.Close()

			if _, err := io.Copy(fout, fin); err != nil {
				return err
			}
		} else {
			destPath := filepath.Join(dir, c.into, filepath.Base(artifact))
			cmd := exec.Command("doas", "-u", "builder", "ln", "-s", artifactPath, destPath)
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("Linking artifact %q to %q: %s", artifactPath, destPath, err)
			}
		}
	}

	return nil
}

type execCommand struct {
	command string
}

type execServer struct {
	cmd      *exec.Cmd
	dir      string
	sources  []string
	waitChan chan error
}

func (c *execCommand) run(dir string) error {
	log.Printf("%s", c.command)
	cmd := exec.Command(
		"doas", "-u", "builder",
		"sh", "-c", fmt.Sprintf("cd %s; %s", dir, c.command),
	)
	env := os.Environ()
	cmd.Env = make([]string, 0, len(env)+1)
	cmd.Env = append(cmd.Env, env...)
	cmd.Env = append(cmd.Env, "WORKDIR="+dir)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("Running %q: %s", c.command, err)
	}
	return nil
}

func (c *execCommand) serve(dir string, sources []string) (server, error) {
	cmd := exec.Command("sh", "-c", c.command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Dir = dir

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	waitChan := make(chan error)
	go func() {
		waitChan <- cmd.Wait()
	}()

	return &execServer{
		cmd:      cmd,
		dir:      dir,
		sources:  sources,
		waitChan: waitChan,
	}, nil
}

func (s *execServer) listSources() []string {
	return s.sources
}

func (s *execServer) kill() {
	if err := syscall.Kill(-s.cmd.Process.Pid, syscall.SIGINT); err != nil {
		log.Print(err)
	}

	if err := s.cmd.Wait(); err != nil {
		log.Print(err)
	}
	log.Printf("Killed")
}

func (s *execServer) wait() chan error {
	return s.waitChan
}

type serveHTTPCommand struct {
	dir  string
	addr string
}

type httpServer struct {
	server  *http.Server
	dir     string
	sources []string
}

func (s *httpServer) listSources() []string {
	return s.sources
}

func (s *httpServer) kill() {
	os.RemoveAll(s.dir)
	s.server.Close()
}

func (s *httpServer) wait() chan error {
	// HTTP server should never die unexpectedly
	return nil
}

func (c *serveHTTPCommand) serve(dir string, sources []string) (server, error) {
	webroot, err := filepath.EvalSymlinks(filepath.Join(dir, c.dir))
	if err != nil {
		return nil, err
	}

	server := &http.Server{
		Addr:    c.addr,
		Handler: http.FileServer(http.Dir(webroot)),
	}

	go func() {
		log.Printf("Serving %s on %s", webroot, c.addr)
		if err := server.ListenAndServe(); err != nil {
			log.Print(err)
		}
	}()

	return &httpServer{
		server:  server,
		dir:     dir,
		sources: sources,
	}, nil
}

func parseCommand(command string) interface{} {
	words := strings.Fields(command)

	if words[0] == "include" {
		cmd := includeCommand{
			artifacts: nil,
			into:      ".",
			copy:      false,
		}

		for i := 1; i < len(words); i++ {
			if words[i] == "-copy" {
				cmd.copy = true
			} else if words[i] == "-into" {
				i++
				cmd.into = words[i]
			} else {
				cmd.artifacts = append(cmd.artifacts, words[i])
			}
		}

		return &cmd
	}

	if words[0] == "serve-http" {
		return &serveHTTPCommand{
			dir:  words[1],
			addr: words[2],
		}
	}

	return &execCommand{
		command: command,
	}
}

func serve(artifact string) (server, error) {
	if !strings.HasPrefix(artifact, "serve-") {
		return nil, fmt.Errorf("Cannot serve recipe %q", artifact)
	}

	recipe, ok := recipes[artifact]
	if !ok {
		return nil, fmt.Errorf("No recipe to serve %q", artifact)
	}

	var sourcesLock sync.Mutex
	sources := make([]string, 0)

	var group errgroup.Group
	for _, command := range recipe {
		include, ok := parseCommand(command).(*includeCommand)
		if !ok {
			continue
		}

		for _, artifact := range include.artifacts {
			artifact := artifact
			group.Go(func() error {
				buildResult, err := build(artifact)
				if err != nil {
					return err
				}

				sourcesLock.Lock()
				defer sourcesLock.Unlock()
				sources = append(sources, buildResult.sources...)
				return nil
			})
		}
	}

	if err := group.Wait(); err != nil {
		return nil, err
	}

	serveDir, err := ioutil.TempDir("", "serve")
	if err != nil {
		return nil, err
	}

	for _, command := range recipe[:len(recipe)-1] {
		if err := parseCommand(command).(runCommand).run(serveDir); err != nil {
			return nil, err
		}
	}

	lastCommand := parseCommand(recipe[len(recipe)-1]).(serveCommand)

	return lastCommand.serve(serveDir, sources)
}

type buildResult struct {
	modTime time.Time
	sources []string
}

var buildFlightGroup singleflight.Group

func build(artifact string) (*buildResult, error) {
	result, err, _ := buildFlightGroup.Do(artifact, func() (interface{}, error) {
		recipe, ok := recipes[artifact]
		if !ok {
			stat, err := os.Stat(artifact)
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("No recipe or source for %q", artifact)
			} else if err != nil {
				return nil, err
			}

			return &buildResult{
				modTime: stat.ModTime(),
				sources: []string{artifact},
			}, nil
		}

		var modTime time.Time
		var needsBuild uint32

		if stat, err := os.Stat(artifact); err == nil {
			modTime = stat.ModTime()
		} else if os.IsNotExist(err) {
			needsBuild = 1
		} else {
			return nil, err
		}

		var sourcesLock sync.Mutex
		sources := make([]string, 0)

		var group errgroup.Group
		for _, command := range recipe {
			include, ok := parseCommand(command).(*includeCommand)
			if !ok {
				continue
			}

			for _, artifact := range include.artifacts {
				artifact := artifact
				group.Go(func() error {
					buildResult, err := build(artifact)
					if err != nil {
						return err
					}

					if buildResult.modTime.After(modTime) {
						atomic.StoreUint32(&needsBuild, 1)
					}

					sourcesLock.Lock()
					defer sourcesLock.Unlock()
					sources = append(sources, buildResult.sources...)
					return nil
				})
			}
		}

		if err := group.Wait(); err != nil {
			return nil, err
		}

		if needsBuild == 0 {
			return &buildResult{
				modTime: modTime,
				sources: sources,
			}, nil
		}

		cmd := exec.Command("doas", "-u", "builder", "mktemp", "-d")
		cmd.Stderr = os.Stderr
		buildDirBytes, err := cmd.Output()
		if err != nil {
			return nil, err
		}

		buildDir := strings.TrimSpace(string(buildDirBytes))
		log.Printf("temp dir is %q", buildDir)

		if err := exec.Command("doas", "-u", "builder", "chmod", "g+rx", buildDir).Run(); err != nil {
			return nil, err
		}

		defer func() {
			if err := exec.Command("doas", "-u", "builder", "rm", "-rf", buildDir).Run(); err != nil {
				log.Printf("Deleting build dir %q: %s", err)
			}
		}()

		for _, command := range recipe {
			if err := parseCommand(command).(runCommand).run(buildDir); err != nil {
				return nil, err
			}
		}

		if err := os.RemoveAll(artifact); err != nil && !os.IsNotExist(err) {
			return nil, err
		}

		sourcePath := filepath.Join(buildDir, filepath.Base(artifact))
		if err := exec.Command(
			"cp", "-r",
			sourcePath,
			artifact,
		).Run(); err != nil {
			return nil, fmt.Errorf("Copying %q to %q: %s", sourcePath, artifact, err)
		}

		return &buildResult{
			modTime: time.Now(),
			sources: sources,
		}, nil
	})

	if result == nil {
		return nil, err
	}

	return result.(*buildResult), err
}

func main() {
	var watching bool
	flag.BoolVar(&watching, "watch", false, "Automatically rebuild files when dependencies change")
	flag.Parse()

	configBytes, err := ioutil.ReadFile("build.yml")
	if err != nil {
		panic(err)
	}

	if err := yaml.Unmarshal(configBytes, &recipes); err != nil {
		panic(err)
	}

	if flag.Arg(0) == "clean" {
		if err := os.RemoveAll(".build"); err != nil {
			panic(err)
		}

		for artifact := range recipes {
			if err := os.RemoveAll(artifact); err != nil {
				panic(err)
			}
		}
		return
	}

	artifacts := flag.Args()

	if len(artifacts) == 0 {
		panic("No artifacts given")
	}

	if err := os.Mkdir(".build", 0700); err != nil && !os.IsExist(err) {
		panic(err)
	}

	var group sync.WaitGroup
	for _, artifact := range artifacts {
		artifact := artifact
		group.Add(1)
		go func() {
			defer group.Done()
			if strings.HasPrefix(artifact, "serve-") {
				log.Printf("Starting up server for %s", artifact)
				server, err := serve(artifact)
				if err != nil {
					panic(err)
				}
				defer func() {
					if server != nil {
						server.kill()
					}
				}()

				watcher, err := fsnotify.NewWatcher()
				if err != nil {
					panic(err)
				}
				defer watcher.Close()

				if err := watcher.Add("build.yml"); err != nil {
					panic(err)
				}

				for _, source := range server.listSources() {
					if err := watcher.Add(source); err != nil {
						panic(err)
					}
				}

				interruptChan := make(chan os.Signal, 1)
				signal.Notify(interruptChan, os.Interrupt)

				for {
					var waitChan chan error
					if server != nil {
						waitChan = server.wait()
					}

					select {
					case e := <-waitChan:
						log.Printf("Server unexpectedly died: %s", e)
						if server != nil {
							for _, source := range server.listSources() {
								watcher.Remove(source)
							}
						}
					case <-interruptChan:
						server.kill()
						return
					case <-watcher.Events:
						if server != nil {
							server.kill()

							for _, source := range server.listSources() {
								watcher.Remove(source)
							}
						}

						log.Printf("Starting up server for %s", artifact)
						server, err = serve(artifact)
						if err != nil {
							log.Printf("%s failed: %s", artifact, err)
						}

						if server != nil {
							for _, source := range server.listSources() {
								if err := watcher.Add(source); err != nil {
									panic(err)
								}
							}
						}
					}
				}
			} else {
				buildResult, err := build(artifact)
				if err != nil {
					panic(err)
				}

				if watching {
					watcher, err := fsnotify.NewWatcher()
					if err != nil {
						panic(err)
					}
					defer watcher.Close()

					if err := watcher.Add("build.yml"); err != nil {
						panic(err)
					}

					for _, source := range buildResult.sources {
						if err := watcher.Add(source); err != nil {
							panic(err)
						}
					}

					interruptChan := make(chan os.Signal, 1)
					signal.Notify(interruptChan, os.Interrupt)

					for {
						select {
						case <-interruptChan:
							return
						case <-watcher.Events:
							for _, source := range buildResult.sources {
								watcher.Remove(source)
							}

							log.Printf("%s: rebuilding...", artifact)
							buildResult, err = build(artifact)
							if err != nil {
								panic(err)
							}
							log.Printf("%s: built", artifact)

							for _, source := range buildResult.sources {
								if err := watcher.Add(source); err != nil {
									panic(err)
								}
							}
						}
					}
				}
			}
		}()
	}

	group.Wait()
}
