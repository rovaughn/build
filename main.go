package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

var config map[string][]string

var originalDir = func() string {
	result, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return result
}()

func tokenize(text string) string {
	hash := sha256.Sum256([]byte(text))
	return hex.EncodeToString(hash[:8])
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

func build(target string) string {
	log.Printf("build %q", target)

	commands, recipeExists := config[target]

	if !recipeExists {
		// If the artifact is present in our source dir, we just include it.
		if stat, err := os.Stat(filepath.Join(originalDir, target)); err == nil {
			artifactKey := tokenize(fmt.Sprintf("%#v%#v", target, stat.ModTime().Unix()))
			artifactPath := filepath.Join(originalDir, ".build", "artifact-"+artifactKey)

			log.Printf("%s -> %s", target, artifactKey)

			if err := os.Symlink(filepath.Join(originalDir, target), artifactPath); err != nil && !os.IsExist(err) {
				panic(err)
			}

			return artifactPath
		} else if !os.IsNotExist(err) {
			panic(err)
		} else {
			panic(fmt.Sprintf("Don't know how to build %q", target))
		}
	}

	dependencies := make(map[string]string)
	orderedDependencies := make([]string, 0)
	keyMaterial := make([]string, 0)

	for _, command := range commands {
		keyMaterial = append(keyMaterial, command)
		switch cmd := interpretCommand(command).(type) {
		case *execCommand:
		case *includeCommand:
			for _, artifact := range cmd.artifacts {
				dependencies[artifact] = build(artifact)
				orderedDependencies = append(orderedDependencies, artifact)
			}
		default:
			panic("bad command")
		}
	}

	sort.Strings(orderedDependencies)

	for _, artifact := range orderedDependencies {
		keyMaterial = append(keyMaterial, artifact, dependencies[artifact])
	}

	artifactKey := tokenize(strings.Join(keyMaterial, strings.Join(keyMaterial, "")))
	artifactPath := filepath.Join(originalDir, ".build", "artifact-"+artifactKey)

	if _, err := os.Stat(artifactPath); err == nil {
		return artifactPath
	} else if !os.IsNotExist(err) {
		panic(err)
	}

	outputPath := filepath.Join(originalDir, ".build", "artifact-output-"+artifactKey)
	defer os.RemoveAll(outputPath)

	buildDir := filepath.Join(originalDir, ".build", "build-"+artifactKey)
	if err := os.Mkdir(buildDir, 0700); err != nil {
		panic(err)
	}
	defer os.RemoveAll(buildDir)

	for _, commandString := range commands {
		switch command := interpretCommand(commandString).(type) {
		case *includeCommand:
			command.into = os.Expand(command.into, func(name string) string {
				if name == "OUT" {
					return outputPath
				} else {
					panic("Unknown variable " + name)
				}
			})

			for _, artifact := range command.artifacts {
				artifactPath, ok := dependencies[artifact]
				if !ok {
					panic("artifact not built somehow")
				}

				var intoPath string
				if filepath.IsAbs(command.into) {
					intoPath = filepath.Join(command.into, artifact)
				} else {
					intoPath = filepath.Join(buildDir, artifact)
				}

				if err := os.Symlink(artifactPath, intoPath); err != nil {
					panic(err)
				}
			}
		case *execCommand:
			log.Printf("Executing %q", command.command)
			cmd := exec.Command("sh", "-c", command.command)
			cmd.Stdout = os.Stderr
			cmd.Stderr = os.Stderr
			cmd.Dir = buildDir
			cmd.Env = append(cmd.Env, "OUT="+outputPath)
			if err := cmd.Run(); err != nil {
				panic(err)
			}
		default:
			panic("bad command")
		}
	}

	if err := os.Rename(outputPath, artifactPath); err != nil {
		panic(err)
	}

	return artifactPath
}

func main() {
	flag.Parse()

	if err := os.MkdirAll(filepath.Join(originalDir, ".build"), 0700); err != nil {
		panic(err)
	}

	configBytes, err := ioutil.ReadFile("build.yml")
	if err != nil {
		panic(err)
	}

	if err := yaml.Unmarshal(configBytes, &config); err != nil {
		panic(err)
	}

	for _, artifact := range flag.Args() {
		artifactPath := build(artifact)
		log.Printf("%s -> %s", artifact, artifactPath)

		if err := os.Remove(artifact); err != nil && !os.IsNotExist(err) {
			panic(err)
		}

		if err := os.Symlink(artifactPath, artifact); err != nil {
			panic(err)
		}
	}
}
