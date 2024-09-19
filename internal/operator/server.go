package operator

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"
	slogecho "github.com/samber/slog-echo"
)

type server struct {
	echo     *echo.Echo
	host     string
	logger   *slog.Logger
	operator *operator
	port     uint
}

type ServerOpts struct {
	Host   string
	Logger *slog.Logger
	Port   uint
}

func NewServer(o *ServerOpts) (*server, error) {
	h := o.Host
	if h == "" {
		h = "127.0.0.1"
	}
	l := o.Logger
	if l == nil {
		l = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	p := o.Port
	if p == 0 {
		p = 8888
	}
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	s := server{
		echo:     e,
		host:     h,
		logger:   l,
		operator: nil,
		port:     p,
	}

	e.Use(slogecho.New(l))
	e.GET("/healthz", s.health)
	return &s, nil
}

func (s *server) health(c echo.Context) error {
	err := s.operator.Health()
	if err != nil {
		return err
	}
	return c.NoContent(http.StatusOK)
}

func (s *server) Run() error {
	a := fmt.Sprintf("%s:%d", s.host, s.port)
	s.logger.Info(fmt.Sprintf("starting server: %s", a))
	return s.echo.Start(a)
}
