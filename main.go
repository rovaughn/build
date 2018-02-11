package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
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

func interpolate(word string, env map[string]string) string {
	if strings.HasPrefix(word, "$") {
		return env[strings.TrimPrefix(word, "$")]
	} else {
		return word
	}
}

func hexify(text string) string {
	hash := sha256.Sum256([]byte(text))
	return hex.EncodeToString(hash[:8])
}

func build(target string, args ...string) string {
	if len(args) == 0 {
		if err := os.Chdir(originalDir); err != nil {
			panic(err)
		}

		if _, err := os.Stat(target); err == nil {
			path, err := filepath.Abs(target)
			if err != nil {
				panic(err)
			}

			return path
		} else if !os.IsNotExist(err) {
			panic(err)
		}
	}

	if _, ok := config[target]; !ok {
		panic(fmt.Sprintf("Don't know how to build %q", target))
	}

	artifactKey := hexify(fmt.Sprintf("%#v%#v", target, args))
	resultPath := filepath.Join(originalDir, ".build", "artifact-"+artifactKey)

	if _, err := os.Stat(resultPath); err == nil {
		return resultPath
	} else if !os.IsNotExist(err) {
		panic(err)
	}

	dir := filepath.Join(originalDir, ".build", "build-"+artifactKey)
	//defer os.RemoveAll(dir)

	if err := os.Mkdir(dir, 0700); err != nil {
		panic(err)
	}

	env := make(map[string]string)

	for i, arg := range args {
		env[fmt.Sprintf("%d", i+1)] = arg
	}

	log.Printf("build %s %v (dir=%s)", target, args, dir)

	for _, command := range config[target] {
		words := strings.Fields(command)

		switch words[0] {
		case "exec":
			for i, word := range words {
				words[i] = interpolate(word, env)
			}

			var stdout = os.Stderr

			if strings.HasPrefix(words[len(words)-1], ">") {
				name := strings.TrimPrefix(words[len(words)-1], ">")
				f, err := os.Create(name)
				if err != nil {
					panic(err)
				}
				defer f.Close()

				stdout = f
				words = words[:len(words)-1]
			}

			log.Printf("exec %s %v >%s", words[1], words[2:], stdout.Name())
			cmd := exec.Command(words[1], words[2:]...)
			cmd.Dir = dir
			cmd.Stdout = stdout
			cmd.Stderr = os.Stderr

			if err := cmd.Run(); err != nil {
				panic(err)
			}
		case "produce":
			if err := os.Chdir(dir); err != nil {
				panic(err)
			}

			if err := os.Rename(words[1], resultPath); err != nil {
				panic(err)
			}
		case "use":
			var target string
			var args []string
			var dest string

			if words[len(words)-2] == "-o" {
				target = words[1]
				args = words[2 : len(words)-2]
				dest = words[len(words)-1]
			} else {
				target = words[1]
				args = words[2:]
				dest = target
			}

			actualArgs := make([]string, 0, len(args))

			if err := os.Chdir(dir); err != nil {
				panic(err)
			}

			for _, arg := range args {
				path, err := filepath.Abs(arg)
				if err != nil {
					panic(err)
				}

				actualArgs = append(actualArgs, path)
			}

			log.Printf("use %s %v -o %s", target, actualArgs, dest)

			result := build(target, actualArgs...)
			if result == "" {
				panic(fmt.Sprintf("%s produced no results", words[1]))
			}

			if err := os.Chdir(dir); err != nil {
				panic(err)
			}

			if err := os.Symlink(result, dest); err != nil {
				panic(err)
			}
		default:
			panic(fmt.Sprintf("Unknown word %s", words[0]))
		}
	}

	return resultPath
}

func main() {
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

	build("frontend")
}
