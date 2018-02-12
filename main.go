package main

import (
	"crypto/sha256"
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
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type cleanupListType struct {
	lock    sync.Mutex
	once    sync.Once
	actions []func()
}

func (c *cleanupListType) add(f func()) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.actions = append(c.actions, f)
}

func (c *cleanupListType) clean() {
	c.once.Do(func() {
		for _, f := range c.actions {
			f()
		}
	})
}

var cleanupList cleanupListType

var flightGroup singleflight.Group
var config map[string][]string

func makeArtifactKey(artifact string) string {
	hash := sha256.Sum256([]byte(artifact))
	humanTag := strings.Map(func(r rune) rune {
		if '0' <= r && r <= '9' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || r == '.' || r == '_' || r == '-' {
			return r
		}

		return '-'
	}, artifact)
	return fmt.Sprintf("%s-%x", humanTag, hash[:8])
}

type commandEnvironment struct {
	buildDir     string
	outputPath   string
	dependencies map[string]string
}

type includeCommand struct {
	artifacts []string
	into      string
}

func (c *includeCommand) run(e commandEnvironment) error {
	absOutputPath, err := filepath.Abs(e.outputPath)
	if err != nil {
		return err
	}

	into := os.Expand(c.into, func(name string) string {
		if name == "OUT" {
			return absOutputPath
		}
		return "$" + name
	})

	for _, artifact := range c.artifacts {
		dependencyPath, ok := e.dependencies[artifact]
		if !ok {
			panic("impossible")
		}

		var intoPath string
		if filepath.IsAbs(into) {
			intoPath = filepath.Join(into, artifact)
		} else {
			intoPath = filepath.Join(e.buildDir, into, artifact)
		}

		absDependecyPath, err := filepath.Abs(dependencyPath)
		if err != nil {
			return err
		}

		if err := os.Symlink(absDependecyPath, intoPath); err != nil {
			return err
		}
	}

	return nil
}

type execCommand struct {
	command string
}

func (c *execCommand) run(e commandEnvironment) error {
	absOutputPath, err := filepath.Abs(e.outputPath)
	if err != nil {
		return err
	}

	cmd := exec.Command("sh", "-c", c.command)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Dir = e.buildDir
	cmd.Env = append(cmd.Env, os.Environ()...)
	cmd.Env = append(cmd.Env, "OUT="+absOutputPath)
	return cmd.Run()
}

func (c *execCommand) runServer(dir string) (*server, error) {
	cmd := exec.Command("sh", "-c", c.command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return &server{
		cmd: cmd,
		dir: dir,
	}, nil
}

type command interface {
	run(commandEnvironment) error
}

func interpretCommand(command string) command {
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

	return &execCommand{
		command: command,
	}
}

type buildResult struct {
	artifactPath string
	modTime      time.Time
	dependencies []string
}

type server struct {
	dependencies []string
	dir          string
	cmd          *exec.Cmd
}

func (s *server) kill() {
	syscall.Kill(-s.cmd.Process.Pid, syscall.SIGINT)
	os.RemoveAll(s.dir)
}

func serveTarget(artifact string) (*server, error) {
	if !strings.HasPrefix(artifact, "serve-") {
		return nil, fmt.Errorf("can't serve %q; it's not a serve- target", artifact)
	}

	artifactKey := makeArtifactKey(artifact)
	sourceDependencies := make([]string, 0)

	recipe, ok := config[artifact]
	if !ok {
		return nil, fmt.Errorf("No recipe for %q", artifact)
	}

	var dependenciesLock sync.Mutex
	dependencies := make(map[string]string)

	var group errgroup.Group
	for _, command := range recipe {
		include, ok := interpretCommand(command).(*includeCommand)
		if !ok {
			continue
		}

		for _, artifact := range include.artifacts {
			artifact := artifact
			group.Go(func() error {
				buildResult, err := build(artifact)
				if err != nil {
					return fmt.Errorf("building %s: %s", artifact, err)
				}

				dependenciesLock.Lock()
				defer dependenciesLock.Unlock()
				dependencies[artifact] = buildResult.artifactPath
				sourceDependencies = append(sourceDependencies, buildResult.dependencies...)

				return nil
			})
		}
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}

	serveDir := filepath.Join(".build", artifactKey)
	if err := os.Mkdir(serveDir, 0700); err != nil {
		return nil, fmt.Errorf("Creating serving directory for %s: %s", artifact, err)
	}

	for _, command := range recipe[:len(recipe)-1] {
		if err := interpretCommand(command).run(commandEnvironment{
			buildDir:     serveDir,
			dependencies: dependencies,
		}); err != nil {
			return nil, err
		}
	}

	command, ok := interpretCommand(recipe[len(recipe)-1]).(*execCommand)
	if !ok {
		return nil, fmt.Errorf("Last command of serve target must be an executable command")
	}

	server, err := command.runServer(serveDir)
	if err != nil {
		return nil, err
	}

	server.dependencies = sourceDependencies

	return server, nil
}

func build(artifact string) (*buildResult, error) {
	if strings.HasPrefix(artifact, "serve-") {
		return nil, fmt.Errorf("Cannot build %q as an artifact", artifact)
	}

	result, err, _ := flightGroup.Do(artifact, func() (interface{}, error) {
		artifactKey := makeArtifactKey(artifact)
		sourceDependencies := make([]string, 0)

		recipe, ok := config[artifact]
		if ok {
			var lastBuildTime time.Time
			var needsBuild bool

			artifactPath := filepath.Join(".build", "artifact-"+artifactKey)

			if stat, err := os.Stat(artifactPath); err == nil {
				lastBuildTime = stat.ModTime()
			} else if os.IsNotExist(err) {
				needsBuild = true
			} else {
				return nil, err
			}

			var dependenciesLock sync.Mutex
			dependencies := make(map[string]string)

			var group errgroup.Group
			for _, command := range recipe {
				include, ok := interpretCommand(command).(*includeCommand)
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

						if buildResult.modTime.After(lastBuildTime) {
							needsBuild = true
						}

						dependenciesLock.Lock()
						defer dependenciesLock.Unlock()
						dependencies[artifact] = buildResult.artifactPath
						sourceDependencies = append(sourceDependencies, buildResult.dependencies...)

						return nil
					})
				}
			}
			if err := group.Wait(); err != nil {
				return nil, err
			}

			if !needsBuild {
				return &buildResult{
					artifactPath: artifactPath,
					modTime:      lastBuildTime,
					dependencies: sourceDependencies,
				}, nil
			}

			buildDir := filepath.Join(".build", "build-"+artifactKey)
			if err := os.Mkdir(buildDir, 0700); err != nil {
				return nil, err
			}
			cleanupList.add(func() {
				os.RemoveAll(buildDir)
			})
			defer os.RemoveAll(buildDir)

			outputPath := filepath.Join(".build", "artifact-output-"+artifactKey)
			cleanupList.add(func() {
				os.RemoveAll(outputPath)
			})
			defer os.RemoveAll(outputPath)

			for _, command := range recipe {
				log.Printf("Executing %q", command)
				if err := interpretCommand(command).run(commandEnvironment{
					buildDir:     buildDir,
					outputPath:   outputPath,
					dependencies: dependencies,
				}); err != nil {
					return nil, err
				}
			}

			if err := os.RemoveAll(artifactPath); err != nil && !os.IsNotExist(err) {
				return nil, err
			}

			if err := os.Rename(outputPath, artifactPath); err != nil {
				return nil, err
			}

			now := time.Now()
			if err := os.Chtimes(artifactPath, now, now); err != nil {
				return nil, err
			}

			return &buildResult{
				artifactPath: artifactPath,
				modTime:      now,
				dependencies: sourceDependencies,
			}, nil
		}

		if stat, err := os.Stat(artifact); err == nil {
			return &buildResult{
				artifactPath: artifact,
				modTime:      stat.ModTime(),
				dependencies: []string{artifact},
			}, nil
		}

		return nil, fmt.Errorf("Don't know how to build %q", artifact)
	})

	if result == nil {
		return nil, err
	}

	return result.(*buildResult), err
}

func buildTarget(artifact string) (interface{}, error) {
	if strings.HasPrefix(artifact, "serve-") {
		return serveTarget(artifact)
	}

	result, err := build(artifact)
	if err != nil {
		return nil, err
	}
	if err := os.Remove(artifact); err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	if err := os.Symlink(result.artifactPath, artifact); err != nil {
		return nil, err
	}

	return result, nil
}

func main() {
	defer cleanupList.clean()

	var watching bool
	flag.BoolVar(&watching, "watch", false, "Keep watching and rebuilding artifact when changes are made.")
	flag.Parse()

	if err := os.Mkdir(".build", 0700); err != nil && !os.IsExist(err) {
		panic(err)
	}

	configBytes, err := ioutil.ReadFile("build.yml")
	if err != nil {
		panic(err)
	}

	if err := yaml.Unmarshal(configBytes, &config); err != nil {
		panic(err)
	}

	if len(flag.Args()) == 0 {
		panic("no targets")
	}

	sources := make([]string, 0)
	servers := make([]*server, 0)
	cleanupList.add(func() {
		for _, server := range servers {
			server.kill()
		}
	})

	sigint := make(chan os.Signal)
	signal.Notify(sigint, os.Interrupt)
	go func() {
		<-sigint
		cleanupList.clean()
		os.Exit(0)
	}()

	for _, artifact := range flag.Args() {
		if _, ok := config[artifact]; !ok {
			panic(fmt.Sprintf("%q isn't a recipe", artifact))
		}

		result, err := buildTarget(artifact)
		if err != nil {
			panic(err)
		}

		switch result := result.(type) {
		case *buildResult:
			sources = append(sources, result.dependencies...)
		case *server:
			sources = append(sources, result.dependencies...)
			servers = append(servers, result)
		default:
			panic("impossible")
		}
	}

	if watching || len(servers) > 0 {
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			panic(err)
		}
		defer watcher.Close()

		log.Printf("Watching %v", sources)

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
					for _, server := range servers {
						log.Printf("Killing server %d...", server.cmd.Process.Pid)
						server.kill()
					}

					servers = servers[:0]

					for _, artifact := range flag.Args() {
						result, err := buildTarget(artifact)
						if err != nil {
							panic(err)
						}

						switch result := result.(type) {
						case *buildResult:
							sources = append(sources, result.dependencies...)
						case *server:
							sources = append(sources, result.dependencies...)
							servers = append(servers, result)
						default:
							panic("impossible")
						}
					}
					log.Printf("Done rebuilding")
				}
			case err := <-watcher.Errors:
				log.Printf("watcher: %s", err)
			}
		}
	}
}
