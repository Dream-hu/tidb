# How to contribute

This document outlines some of the conventions on development workflow, commit
message formatting, contact points and other resources to make it easier to get
your contribution accepted.

## Getting started

- Fork the repository on GitHub.
- Read the README.md for build instructions.
- Play with the project, submit bugs, submit patches!

## Building BR

Developing BR requires:

* [Go 1.23+](http://golang.org/doc/code.html)
* An internet connection to download the dependencies

1. Navigate to the tidb directory

```sh
cd ../tidb
```

2. run `make build_br` to build the program.

```sh
make build_br
```

After all these, you will find `br` in `tidb/bin` directory

### Running tests

This project contains unit tests and integration tests with coverage collection.
See [tests/README.md](./tests/README.md) for how to execute and add tests.

### Updating dependencies

BR uses [Go 1.11 module](https://github.com/golang/go/wiki/Modules) to manage dependencies.
To add or update a dependency: use the `go mod edit` command to change the dependency.

## Contribution flow

This is a rough outline of what a contributor's workflow looks like:

- Create a topic branch from where you want to base your work. This is usually `master`.
- Make commits of logical units and add test case if the change fixes a bug or adds new functionality.
- Run tests and make sure all the tests are passed.
- Make sure your commit messages are in the proper format (see below).
- Push your changes to a topic branch in your fork of the repository.
- Submit a pull request.
- Your PR must receive LGTMs from two maintainers.

Thanks for your contributions!

### Code style

The coding style suggested by the Golang community is used in BR.
See the [style doc](https://github.com/golang/go/wiki/CodeReviewComments) for details.

Please follow this style to make BR easy to review, maintain and develop.

### Format of the Commit Message

We follow a rough convention for commit messages that is designed to answer two
questions: what changed and why. The subject line should feature the what and
the body of the commit should describe the why.

```
restore: add comment for variable declaration

Improve documentation.
```

The format can be described more formally as follows:

```
<subsystem>: <what changed>
<BLANK LINE>
<why this change was made>
<BLANK LINE>
<footer>(optional)
```

The first line is the subject and should be no longer than 70 characters, the
second line is always blank, and other lines should be wrapped at 80 characters.
This allows the message to be easier to read on GitHub as well as in various
git tools.

If the change affects more than one subsystem, you can use comma to separate them like `backup,restore:`.

If the change affects many subsystems, you can use ```*``` instead, like ```*:```.

For the why part, if no specific reason for the change,
you can use one of some generic reasons like "Improve documentation.",
"Improve performance.", "Improve robustness.", "Improve test coverage."