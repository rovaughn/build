build
=====

`build` is a pretty simple build tool meant to be an upgrade to make.  It
focuses on correcting the following deficiencies I have run into with make:

- If a build recipe fails halfway through, a partially made and incorrect
  artifact will still be produced.  Now make will think the artifact was made
  correctly and not rebuild it.
- If the build configuration (i.e. Makefile) changes, targets won't be remade
  using the new recipes.
- Inputs/outputs of recipes are relatively ad hoc.  You can forget a dependency
  or, and a recipe can produce more artifacts than it intended to.
- Parallelism must be manually enabled, and due to the lack of isolation in
  build steps, potentially error prone.
- No support for automatically cleaning up build artifacts.
- No support for watching artifacts, automatically rebuilding them when
  dependencies change.
- No support for running long running processes from artifacts, e.g. an HTTP
  server, that automatically restarts when dependencies change.

Here is what the build configuration looks like, stored in a file called
`build.yml`:

	index.css:
		- include node_modules index.scss
		- node_modules/.bin/sass index.scss >index.css

	bundle.js:
		- include node_modules app.ts
		- node_modules/.bin/tslint app.ts
		- node_modules/.bin/tsc --strictNullChecks --noImplicitAny app.ts
		- browserify app.js >bundle.js

	dist:
		- mkdir dist
		- include index.html index.css bundle.js -copy -into dist

	api:
		- include main.go
		- go vet
		- go build -o api

	api.zip:
		- include api
		- zip -9 api.zip api

	serve-dist:
		- include dist
		- serve-http dist :8000

	serve-api:
		- include api
		- ./api -addr :8001

When a recipe is run, it gets its own private temporary build directory, and
can only see artifacts and source files it explicitly includes.  The build tool
uses the include commands to determine the recipe's dependencies, and since the
recipe can only see files it explicitly includes, you can't forget to
explicitly mention a dependency.

The recipe's output is determined by whatever file is left in the build
directory with the artifact's name.  For instance, the recipe for index.css
produces the file index.css.  If the recipe succeeds, this file will be
atomically moved to the real artifact.  If the recipe fails, no artifact will
be produced at all.  An artifact can be a file or directory.

Some recipes can be special serving recipes; these recipes expect their last
command to be a long running command.  It can either be a typical shell command
that starts a server, or the special built in serve-http command.  A server
command will automatically restart and rebuild when changes are made to any of
its sources.

Here are some examples of what the command line could look like:

	# Simply build api.zip and dist, if necessary.
	$ build api.zip dist

	# Automatically rebuilds dist whenever changes are made to sources files.
	$ build -watch dist

	# Serve the dist folder over HTTP; automatically rebuild it whenever its
	# sources change.
	$ build serve-dist

	# Run both serve commands at the same time.  They will be independently
	# restarted/rebuilt if their sources change.
	$ build serve-api serve-dist
