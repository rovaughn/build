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
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var flightGroup singleflight.Group
var config map[string][]string

func makeArtifactKey(artifact string) string {
	hash := sha256.Sum256([]byte(artifact))
	humanTag := strings.Map(func(r rune) rune {
		if '0' <= r && r <= '9' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || r == '.' || r == '_' || r == '-' {
			return r
		} else {
			return '-'
		}
	}, artifact)
	return fmt.Sprintf("%s-%x", humanTag, hash[:8])
}

type includeCommand struct {
	artifacts []string
	into      string
}

type execCommand struct {
	command string
}

func interpretCommand(command string) interface{} {
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
	} else {
		return &execCommand{
			command: command,
		}
	}
}

type buildResult struct {
	artifactPath string
	modTime      time.Time
	dependencies []string
}

func build(artifact string) (*buildResult, error) {
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

			for _, command := range recipe {
				include, ok := interpretCommand(command).(*includeCommand)
				if !ok {
					continue
				}

				var group errgroup.Group
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
				if err := group.Wait(); err != nil {
					return nil, err
				}
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
			defer os.RemoveAll(buildDir)

			outputPath := filepath.Join(".build", "artifact-output-"+artifactKey)
			defer os.RemoveAll(outputPath)

			absOutputPath, err := filepath.Abs(outputPath)
			if err != nil {
				return nil, err
			}

			for _, command := range recipe {
				log.Printf("Executing %q", command)
				switch command := interpretCommand(command).(type) {
				case *includeCommand:
					command.into = os.Expand(command.into, func(name string) string {
						if name == "OUT" {
							return absOutputPath
						} else {
							return "$" + name
						}
					})

					for _, artifact := range command.artifacts {
						dependencyPath, ok := dependencies[artifact]
						if !ok {
							panic("impossible")
						}

						var intoPath string
						if filepath.IsAbs(command.into) {
							intoPath = filepath.Join(command.into, artifact)
						} else {
							intoPath = filepath.Join(buildDir, command.into, artifact)
						}

						absDependecyPath, err := filepath.Abs(dependencyPath)
						if err != nil {
							return nil, err
						}

						if err := os.Symlink(absDependecyPath, intoPath); err != nil {
							return nil, err
						}
					}
				case *execCommand:
					absOutputPath, err := filepath.Abs(outputPath)
					if err != nil {
						return nil, err
					}

					cmd := exec.Command("sh", "-c", command.command)
					cmd.Stdout = os.Stderr
					cmd.Stderr = os.Stderr
					cmd.Dir = buildDir
					cmd.Env = append(cmd.Env, os.Environ()...)
					cmd.Env = append(cmd.Env, "OUT="+absOutputPath)
					if err := cmd.Run(); err != nil {
						return nil, err
					}
				default:
					panic("impossible")
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

	if err != nil {
		return nil, err
	} else {
		return result.(*buildResult), nil
	}
}

func buildTarget(artifact string) (*buildResult, error) {
	buildResult, err := build(artifact)
	if err != nil {
		return nil, err
	}

	if err := os.Remove(artifact); err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	if err := os.Symlink(buildResult.artifactPath, artifact); err != nil {
		return nil, err
	}

	return buildResult, nil
}

func main() {
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

	sources := make([]string, 0)

	for _, artifact := range flag.Args() {
		if _, ok := config[artifact]; !ok {
			panic(fmt.Sprintf("%q isn't a recipe", artifact))
		}

		buildResult, err := buildTarget(artifact)
		if err != nil {
			panic(err)
		}

		sources = append(sources, buildResult.dependencies...)
	}

	if watching {
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
					for _, artifact := range flag.Args() {
						_, err := buildTarget(artifact)
						if err != nil {
							panic(err)
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
