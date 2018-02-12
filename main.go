package main

import (
	"flag"
	"fmt"
	"github.com/fsnotify/fsnotify"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var recipes map[string][]string

type command interface {
	run(dir string) error
}

type includeCommand struct {
	artifacts []string
	into      string
}

func (c *includeCommand) run(dir string) error {
	for _, artifact := range c.artifacts {
		artifactPath, err := filepath.Abs(artifact)
		if err != nil {
			return err
		}

		if err := os.Symlink(artifactPath, filepath.Join(dir, c.into, artifact)); err != nil {
			return err
		}
	}

	return nil
}

type execCommand struct {
	command string
}

func (c *execCommand) run(dir string) error {
	cmd := exec.Command("sh", "-c", c.command)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Dir = dir
	return cmd.Run()
}

func parseCommand(command string) command {
	words := strings.Fields(command)

	if words[0] == "include" {
		cmd := includeCommand{
			artifacts: nil,
			into:      ".",
		}

		for i := 1; i < len(words); i++ {
			if words[i] == "-into" {
				i++
				cmd.into = words[i]
			} else {
				cmd.artifacts = append(cmd.artifacts, words[i])
			}
		}

		return &cmd
	}

	//if words[0] == "serve-http" {
	//	return &httpCommand{
	//		dir:  words[1],
	//		addr: words[2],
	//	}
	//}

	return &execCommand{
		command: command,
	}
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

		buildDir, err := ioutil.TempDir(".build", "build")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(buildDir)

		for _, command := range recipe {
			if err := parseCommand(command).run(buildDir); err != nil {
				return nil, err
			}
		}

		if err := os.RemoveAll(artifact); err != nil && !os.IsNotExist(err) {
			return nil, err
		}

		if err := os.Rename(filepath.Join(buildDir, filepath.Base(artifact)), artifact); err != nil {
			return nil, err
		}

		// TODO is this necessary?  Might be necessary for directories.
		now := time.Now()
		if err := os.Chtimes(artifact, now, now); err != nil {
			return nil, err
		}

		return &buildResult{
			modTime: now,
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

	sources := make([]string, 0)

	for _, artifact := range artifacts {
		buildResult, err := build(artifact)
		if err != nil {
			panic(err)
		}

		sources = append(sources, buildResult.sources...)
	}

	if watching {
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			panic(err)
		}
		defer watcher.Close()

		for _, source := range sources {
			if err := watcher.Add(source); err != nil {
				panic(err)
			}
		}

		for {
			log.Printf("Watching for changes...")
			select {
			case event := <-watcher.Events:
				if event.Op&fsnotify.Rename != 0 {
					if err := watcher.Add(event.Name); err != nil {
						panic(err)
					}
				}

				if event.Op&(fsnotify.Write|fsnotify.Rename) != 0 {
					log.Printf("%q changed, rebuilding...", event.Name)

					for _, artifact := range artifacts {
						_, err := build(artifact)
						if err != nil {
							panic(err)
						}
					}
				}
			case err := <-watcher.Errors:
				log.Printf("watcher: %s", err)
			}
		}
	}
}
