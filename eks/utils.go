package eks

import (
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"reflect"
	"sync"
	"time"

	"go.uber.org/zap"
)

func catchInterrupt(lg *zap.Logger, stopc chan struct{}, once *sync.Once, sigc chan os.Signal, run func() error) (err error) {
	errc := make(chan error)
	go func() {
		errc <- run()
	}()
	select {
	case <-stopc:
		lg.Info("interrupting")
		gerr := <-errc
		lg.Info("interrupted", zap.Error(gerr))
		err = fmt.Errorf("interrupted (run function returned %v)", gerr)
	case sig := <-sigc:
		once.Do(func() { close(stopc) })
		err = fmt.Errorf("received os signal %v (interrupted %v)", sig, <-errc)
	case err = <-errc:
	}
	return err
}

var httpFileTransport *http.Transport

func init() {
	httpFileTransport = new(http.Transport)
	httpFileTransport.RegisterProtocol("file", http.NewFileTransport(http.Dir("/")))
}

// curl -L [URL] | writer
func httpDownloadFile(lg *zap.Logger, u string, wr io.Writer) error {
	lg.Info("downloading", zap.String("url", u))
	cli := &http.Client{Transport: httpFileTransport}
	r, err := cli.Get(u)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode >= 400 {
		return fmt.Errorf("%q returned %d", u, r.StatusCode)
	}

	_, err = io.Copy(wr, r.Body)
	if err != nil {
		lg.Warn("failed to download", zap.String("url", u), zap.Error(err))
	} else {
		if f, ok := wr.(*os.File); ok {
			lg.Info("downloaded",
				zap.String("url", u),
				zap.String("file-path", f.Name()),
			)
		} else {
			lg.Info("downloaded",
				zap.String("url", u),
				zap.String("value-of", reflect.ValueOf(wr).String()),
			)
		}
	}
	return err
}

const ll = "0123456789abcdefghijklmnopqrstuvwxyz"

func randString(n int) string {
	b := make([]byte, n)
	for i := range b {
		rand.Seed(time.Now().UnixNano())
		b[i] = ll[rand.Intn(len(ll))]
	}
	return string(b)
}
