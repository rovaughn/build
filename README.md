build
=====

Simple build tool to replace make.

Goals/principles
----------------

- Simple language for defining build dependencies.  In this case, YAML + shell
  commands.
- Atomic build steps.  Intermediate build products should be kept out of the
  way.  If a build step fails midway through it won't dump the partial created
  contents.
- Built in watching/serving.  Easy to watch dependencies and automatically
  rebuild and possibly restart a server if changes are made.
- Efficient.  Parallel where possible.
- Dependencies can be identified after a build step is done.  Can even be
  automatically determined from kernel traces.
