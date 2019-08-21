// Copyright 2019 Netflix, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package command

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Netflix/p2plab/errdefs"
	"github.com/Netflix/p2plab/pkg/httputil"
	"github.com/Netflix/p2plab/pkg/logutil"
	"github.com/Netflix/p2plab/printer"
	cleanhttp "github.com/hashicorp/go-cleanhttp"
	opentracing "github.com/opentracing/opentracing-go"
	tlog "github.com/opentracing/opentracing-go/log"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	jaeger "github.com/uber/jaeger-client-go"
	"github.com/urfave/cli"
)

type OutputType string

var (
	OutputUnix OutputType = "unix"
	OutputJSON OutputType = "json"
)

func AttachAppContext(ctx context.Context, app *cli.App) {
	tracer, closer := getTracer()

	var span opentracing.Span

	for i, cmd := range app.Commands {
		for j, subcmd := range cmd.Subcommands {
			func(before cli.BeforeFunc) {
				name := subcmd.Name
				app.Commands[i].Subcommands[j].Before = func(c *cli.Context) error {
					if before != nil {
						if err := before(c); err != nil {
							return err
						}
					}

					span = tracer.StartSpan(name)
					span.LogFields(tlog.String("command", strings.Join(os.Args, " ")))

					logger, writer, err := newLogger(c)
					if err != nil {
						return err
					}
					ctx = logger.WithContext(ctx)
					ctx = logutil.WithLogWriter(ctx, writer)
					ctx = opentracing.ContextWithSpan(ctx, span)

					c.App.Metadata["context"] = ctx
					return nil
				}
			}(subcmd.Before)
		}
	}

	after := app.After
	app.After = func(c *cli.Context) error {
		if after != nil {
			if err := after(c); err != nil {
				return err
			}
		}

		if span != nil {
			span.Finish()
		}
		return closer.Close()
	}
}

func AttachAppPrinter(app *cli.App) {
	app.Before = joinBefore(app.Before, func(c *cli.Context) error {
		output := OutputType(c.String("output"))

		var p printer.Printer
		switch output {
		case OutputUnix:
			p = printer.NewUnixPrinter()
		case OutputJSON:
			p = printer.NewJSONPrinter()
		default:
			return fmt.Errorf("output %q is not valid", output)
		}

		c.App.Metadata["printer"] = p
		return nil
	})
}

func AttachAppClient(app *cli.App) {
	app.Before = joinBefore(app.Before, func(c *cli.Context) error {
		var opts []httputil.ClientOption
		if c.GlobalString("log-level") == "debug" {
			logger, _, err := newLogger(c)
			if err != nil {
				return err
			}

			opts = append(opts, httputil.WithLogger(logger))
		}

		client, err := httputil.NewClient(cleanhttp.DefaultClient(), opts...)
		if err != nil {
			return err
		}

		app.Metadata["client"] = client
		return nil
	})
}

func joinBefore(fns ...cli.BeforeFunc) cli.BeforeFunc {
	return func(c *cli.Context) error {
		for _, fn := range fns {
			if fn == nil {
				continue
			}

			err := fn(c)
			if err != nil {
				return err
			}
		}
		return nil
	}
}

func CommandContext(c *cli.Context) context.Context {
	return c.App.Metadata["context"].(context.Context)
}

func CommandPrinter(c *cli.Context) printer.Printer {
	return c.App.Metadata["printer"].(printer.Printer)
}

func CommandClient(c *cli.Context) *httputil.Client {
	return c.App.Metadata["client"].(*httputil.Client)
}

func getTracer() (opentracing.Tracer, io.Closer) {
	if traceAddr := os.Getenv("JAEGER_TRACE"); traceAddr != "" {
		tr, err := jaeger.NewUDPTransport(traceAddr, 0)
		if err != nil {
			panic(err)
		}

		return jaeger.NewTracer(
			"labctl",
			jaeger.NewConstSampler(true),
			jaeger.NewRemoteReporter(tr),
		)
	}

	return opentracing.NoopTracer{}, &nopCloser{}
}

type nopCloser struct{}

func (*nopCloser) Close() error {
	return nil
}

func newLogger(c *cli.Context) (*zerolog.Logger, io.Writer, error) {
	var out io.Writer
	switch c.GlobalString("log-writer") {
	case "console":
		out = zerolog.ConsoleWriter{Out: os.Stderr}
	case "json":
		out = os.Stderr
	default:
		return nil, nil, errors.Wrapf(errdefs.ErrInvalidArgument, "unknown log writer %q", c.GlobalString("log-writer"))
	}

	level, err := zerolog.ParseLevel(c.GlobalString("log-level"))
	if err != nil {
		return nil, nil, err
	}

	logger := zerolog.New(out).
		Level(level).
		With().Timestamp().Logger()

	return &logger, out, nil
}

func ExtractNameFromFilename(filename string) string {
	return strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename))
}
