// Доработать программу из практической части так, чтобы при отправке ей сигнала SIGUSR1 она увеличивала глубину поиска на 2.
// Добавить общий таймаут на выполнение следующих операций: работа парсера, получений ссылок со страницы, формирование заголовка.
// Need more time...
package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/PuerkitoBio/goquery"
)

type CrawlResult struct {
	Err   error
	Title string
	Url   string
}

type Page interface {
	GetTitle() string
	GetLinks() []string
}

type page struct {
	doc *goquery.Document
}

func NewPage(raw io.Reader) (Page, error) {
	doc, err := goquery.NewDocumentFromReader(raw)
	if err != nil {
		return nil, err
	}
	return &page{doc: doc}, nil
}

func (p *page) GetTitle() string {
	return p.doc.Find("title").First().Text()
}

func (p *page) GetLinks() []string {
	var urls []string
	p.doc.Find("a").Each(func(_ int, s *goquery.Selection) {
		url, ok := s.Attr("href")
		if ok {
			urls = append(urls, url)
		}
	})
	return urls
}

type Requester interface {
	Get(ctx context.Context, url string) (Page, error)
}

type requester struct {
	timeout time.Duration
	logger  *zap.Logger
}

func NewRequester(timeout time.Duration, logger *zap.Logger) requester {
	return requester{timeout: timeout, logger: logger}
}

func (r requester) Get(ctx context.Context, url string) (Page, error) {
	r.logger.Debug("started", zap.String("url", url), zap.String("method", "requester"))
	defer r.logger.Debug("finished", zap.String("url", url), zap.String("method", "requester"))

	select {
	case <-ctx.Done():
		r.logger.Debug("ctx done", zap.String("url", url), zap.String("method", "requester"))
		return nil, nil
	default:
		cl := &http.Client{
			Timeout: r.timeout,
		}
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			r.logger.Debug("Error by creating new request", zap.String("url", url), zap.String("err", err.Error()), zap.String("method", "requester"))
			return nil, err
		}
		body, err := cl.Do(req)
		if err != nil {
			r.logger.Debug("Error by sending request", zap.String("url", url), zap.String("err", err.Error()), zap.String("method", "requester"))
			return nil, err
		}
		defer body.Body.Close()
		page, err := NewPage(body.Body)
		if err != nil {
			r.logger.Debug("Error by creating new page", zap.String("url", url), zap.String("err", err.Error()), zap.String("method", "requester"))
			return nil, err
		}
		return page, nil
	}
	return nil, nil
}

//Crawler - интерфейс (контракт) краулера
type Crawler interface {
	Scan(ctx context.Context, url string, depth int)
	ChanResult() <-chan CrawlResult
	IncDepth()
}

type crawler struct {
	r        Requester
	logger   zap.logger
	res      chan CrawlResult
	visited  map[string]struct{}
	mu       sync.RWMutex
	maxDepth int
}

func NewCrawler(r Requester, depth int, logger *zap.Logger) *crawler {
	return &crawler{
		r:        r,
		logger:   *zap.Logger,
		res:      make(chan CrawlResult),
		visited:  make(map[string]struct{}),
		mu:       sync.RWMutex{},
		maxDepth: depth,
	}
}

func (c *crawler) Scan(ctx context.Context, url string, depth int) {
	c.logger.Debug("started", zap.String("url", url), zap.Int("depth", depth), zap.String("method", "crawler"))
	defer c.logger.Debug("finished", zap.String("url", url), zap.Int("depth", depth), zap.String("method", "crawler"))

	if depth <= 0 { //Проверяем то, что есть запас по глубине
		return
	}
	c.mu.RLock()
	_, ok := c.visited[url] //Проверяем, что мы ещё не смотрели эту страницу
	c.mu.RUnlock()
	if ok {
		return
	}
	select {
	case <-ctx.Done(): //Если контекст завершен - прекращаем выполнение
		c.logger.Debug("ctx was done", zap.String("url", url), zap.String("method", "crawler"))
		return
	default:
		page, err := c.r.Get(ctx, url) //Запрашиваем страницу через Requester
		if err != nil {
			c.logger.Error("Error with getting page", zap.String("url", url), zap.String("method", "crawler"), zap.String("err", err.Error()))
			c.res <- CrawlResult{Err: err} //Записываем ошибку в канал
			return
		}
		c.mu.Lock()
		c.visited[url] = struct{}{} //Помечаем страницу просмотренной
		c.mu.Unlock()
		c.res <- CrawlResult{ //Отправляем результаты в канал
			Title: page.GetTitle(),
			Url:   url,
		}
		for _, link := range page.GetLinks() {
			go c.Scan(ctx, link, depth-1) //На все полученные ссылки запускаем новую рутину сборки
		}
	}
}

func (c *crawler) ChanResult() <-chan CrawlResult {
	return c.res
}

func (c *crawler) IncDepth() {
	c.mu.Lock()
	c.maxDepth += 2
	c.mu.Unlock()
}

//Config - структура для конфигурации
type Config struct {
	MaxDepth   int
	MaxResults int
	MaxErrors  int
	Url        string
	Timeout    int //in seconds
	LogLevel   int
}

func main() {
	cfg := Config{
		MaxDepth:   3,
		MaxResults: 10,
		MaxErrors:  5,
		Url:        "https://telegram.org",
		Timeout:    10,
		LogLevel:   zapcore.InfoLevel,
	}
	var cr Crawler
	var r Requester

	logCfg := zap.Config{
		Level:       zap.NewAtomicLevelAt(zapcore.Level(cfg.LogLevel)),
		Development: false,
		Sampling: &zap.SamplingConfig{
			Initial:    100,
			Thereafter: 100,
		},
		Encoding:         "console",
		EncoderConfig:    zap.NewDevelopmentEncoderConfig(),
		OutputPaths:      []string{"stdout"},
		ErrorOutputPaths: []string{"stderr"},
	}

	log, _ := logCfg.Build()
	defer log.Sync() // flushes buffer, if any

	pidLog := log.With(zap.Int("pid", os.Getpid()))

	pidLog.Debug("start")

	r = NewRequester(time.Duration(cfg.Timeout)*time.Second, pidLog)
	cr = NewCrawler(r, cfg.MaxDepth, pidLog)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.Timeout)*time.Second)
	go cr.Scan(ctx, cfg.Url, cfg.MaxDepth)         //Запускаем краулер в отдельной рутине
	go processResult(ctx, cancel, cr, cfg, pidLog) //Обрабатываем результаты в отдельной рутине

	sigCh := make(chan os.Signal)        //Создаем канал для приема сигналов
	signal.Notify(sigCh, syscall.SIGINT) //Подписываемся на сигнал SIGINT
	for {
		select {
		case <-ctx.Done(): //Если всё завершили - выходим
			return
		case sig := <-sigCh:
			switch sig {
			case syscall.SIGINT:
				pidLog.Debug("Got SIGINT")
				cancel() //Если пришёл сигнал SigInt - завершаем контекст
			case syscall.SIGUSR1:
				pidLog.Debug("Got SIGUSR1")
				cr.IncDepth() //Если пришёл сигнал SIGUSR1 - увеличим глубину на 2

			}
		}

	}
}

func processResult(ctx context.Context, cancel func(), cr Crawler, cfg Config, log *zap.Logger) {
	var maxResult, maxErrors = cfg.MaxResults, cfg.MaxErrors
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-cr.ChanResult():
			if msg.Err != nil {
				maxErrors--
				log.Error("crawler error", zap.Int("maxErrors", maxErrors), zap.String("err", msg.Err.Error()))
				if maxErrors <= 0 {
					cancel()
					return
				}
			} else {
				maxResult--
				log.Info("crawler result", zap.Int("maxResult", maxErrors), zap.String("url", msg.Url), zap.String("title", msg.Title))
				if maxResult <= 0 {
					cancel()
					return
				}
			}
		}
	}
}
