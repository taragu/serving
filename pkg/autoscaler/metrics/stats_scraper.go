/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metrics

import (
	"context"
	"errors"
	"fmt"
	"k8s.io/apimachinery/pkg/labels"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.opencensus.io/stats"
	"go.opencensus.io/stats/view"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	corev1listers "k8s.io/client-go/listers/core/v1"
	"knative.dev/pkg/logging"
	pkgmetrics "knative.dev/pkg/metrics"
	av1alpha1 "knative.dev/serving/pkg/apis/autoscaling/v1alpha1"
	"knative.dev/serving/pkg/apis/networking"
	"knative.dev/serving/pkg/apis/serving"
	"knative.dev/serving/pkg/metrics"
	"knative.dev/serving/pkg/resources"
)

const (
	httpClientTimeout = 3 * time.Second

	// scraperPodName is the name used in all stats sent from the scraper to
	// the autoscaler. The actual customer pods are hidden behind the scraper. The
	// autoscaler does need to know how many customer pods are reporting metrics.
	// Instead, the autoscaler knows the stats it receives are either from the
	// scraper or the activator.
	scraperPodName = "service-scraper"

	// scraperMaxRetries are retries to be done to the actual Scrape routine. We want
	// to retry if a Scrape returns an error or if the Scrape goes to a pod we already
	// scraped.
	scraperMaxRetries = 10
)

var (
	// ErrFailedGetEndpoints specifies the error returned by scraper when it fails to
	// get endpoints.
	ErrFailedGetEndpoints = errors.New("failed to get endpoints")

	// ErrDidNotReceiveStat specifies the error returned by scraper when it does not receive
	// stat from an unscraped pod
	ErrDidNotReceiveStat = errors.New("did not receive stat from an unscraped pod")

	// Sentinel error to retrun from pod scraping routine, when we could not
	// scrape even a single pod.
	errNoPodsScraped = errors.New("no pods scraped")
	errPodsExhausted = errors.New("pods exhausted")

	//scrapeTimeM = stats.Float64(
	//	"scrape_time",
	//	"Time to scrape metrics in milliseconds",
	//	stats.UnitMilliseconds)
	bulkScrapeTimeM = stats.Float64(
		"bulk_scrape_time",
		"Time to bulk scrape metrics in milliseconds",
		stats.UnitMilliseconds)
	reportBulkScrapeTimeM = stats.Float64(
		"report_bulk_scrape_time",
		"Time to report bulk scraped metrics in milliseconds",
		stats.UnitMilliseconds)
)

func init() {
	if err := view.Register(
		//&view.View{
		//	Description: "The time to scrape metrics in milliseconds",
		//	Measure:     scrapeTimeM,
		//	Aggregation: view.Distribution(pkgmetrics.Buckets125(1, 100000)...),
		//	TagKeys:     metrics.CommonRevisionKeys,
		//},
		&view.View{
			Description: "The time to bulk scrape metrics in milliseconds",
			Measure:     bulkScrapeTimeM,
			Aggregation: view.Distribution(pkgmetrics.Buckets125(1, 100000)...),
			TagKeys:     metrics.CommonRevisionKeys,
		}, &view.View{
			Description: "The time to report bulk scraped metrics in milliseconds",
			Measure:     reportBulkScrapeTimeM,
			Aggregation: view.Distribution(pkgmetrics.Buckets125(1, 100000)...),
			TagKeys:     metrics.CommonRevisionKeys,
		},
	); err != nil {
		panic(err)
	}
}

// StatsScraper defines the interface for collecting Revision metrics
type StatsScraper interface {
	// Scrape scrapes the Revision queue metric endpoint. The duration is used
	// to cutoff young pods, whose stats might skew lower.
	Scrape(time.Duration) (Stat, error)

	// BulkScrape collects stats from DaemonSet scraper pod and stores the stats.
	BulkScrape(context.Context)

	Report(*av1alpha1.Metric) (Stat, error)
}

// scrapeClient defines the interface for collecting Revision metrics for a given
// URL. Internal used only.
type scrapeClient interface {
	// Scrape scrapes the given URL.
	Scrape(url string) (Stat, error)
	// BulkScrape scrapes a given DaemonSet scraper endpoint and returns a map of
	// revision name to stats, as well as error.
	BulkScrape(url string) (map[string][]Stat, error)
}

// cacheDisabledClient is a http client with cache disabled. It is shared by
// every goruntime for a revision scraper.
var cacheDisabledClient = &http.Client{
	Transport: &http.Transport{
		// Do not use the cached connection
		DisableKeepAlives: true,
	},
	Timeout: httpClientTimeout,
}

// serviceScraper scrapes Revision metrics via a K8S service by sampling. Which
// pod to be picked up to serve the request is decided by K8S. Please see
// https://kubernetes.io/docs/concepts/services-networking/network-policies/
// for details.
type serviceScraper struct {
	sClient  scrapeClient
	counter  resources.EndpointsCounter
	url      string
	statsCtx context.Context
	logger   *zap.SugaredLogger

	podAccessor     resources.PodAccessor
	podsAddressable bool

	tickProvider func(time.Duration) *time.Ticker
}

type bulkScraper struct {
	podLister       corev1listers.PodLister
	endpointsLister corev1listers.EndpointsLister
	sClient         scrapeClient
	revMap          sync.Map
	logger          *zap.SugaredLogger
}

// NewStatsScraper creates a new StatsScraper for the Revision which
// the given Metric is responsible for.
func NewStatsScraper(metric *av1alpha1.Metric, counter resources.EndpointsCounter,
	podAccessor resources.PodAccessor, logger *zap.SugaredLogger, tickProvider func(time.Duration) *time.Ticker) (StatsScraper, error) {
	sClient, err := NewHTTPScrapeClient(cacheDisabledClient)
	if err != nil {
		return nil, err
	}
	return newServiceScraperWithClient(metric, counter, podAccessor, sClient, logger, tickProvider)
}

func (s *serviceScraper) Report(metric *av1alpha1.Metric) (Stat, error) {
	return emptyStat, nil
}

func (s *serviceScraper) BulkScrape(ctx context.Context) {
}

func NewBulkScraper(podLister corev1listers.PodLister, endpointsLister corev1listers.EndpointsLister, logger *zap.SugaredLogger) (StatsScraper, error) {
	sClient, err := NewHTTPScrapeClient(cacheDisabledClient)
	if err != nil {
		return nil, err
	}
	return newBulkScraperWithClient(podLister, endpointsLister, sClient, logger)
}

func newBulkScraperWithClient(
	podLister corev1listers.PodLister,
	endpointsLister corev1listers.EndpointsLister,
	sClient scrapeClient,
	logger *zap.SugaredLogger) (*bulkScraper, error) {
	if podLister == nil {
		return nil, errors.New("pod lister must not be nil")
	}
	if endpointsLister == nil {
		return nil, errors.New("endpoints lister must not be nil")
	}
	if sClient == nil {
		return nil, errors.New("scrape client must not be nil")
	}
	return &bulkScraper{
		sClient:         sClient,
		podLister:       podLister,
		endpointsLister: endpointsLister,
		revMap:          sync.Map{},
		logger:          logger,
	}, nil
}

func newServiceScraperWithClient(
	metric *av1alpha1.Metric,
	counter resources.EndpointsCounter,
	podAccessor resources.PodAccessor,
	sClient scrapeClient,
	logger *zap.SugaredLogger,
	tickProvider func(time.Duration) *time.Ticker) (*serviceScraper, error) {
	if metric == nil {
		return nil, errors.New("metric must not be nil")
	}
	if counter == nil {
		return nil, errors.New("counter must not be nil")
	}
	if sClient == nil {
		return nil, errors.New("scrape client must not be nil")
	}
	revName := metric.Labels[serving.RevisionLabelKey]
	if revName == "" {
		return nil, errors.New("no Revision label found for Metric " + metric.Name)
	}
	svcName := metric.Labels[serving.ServiceLabelKey]
	cfgName := metric.Labels[serving.ConfigurationLabelKey]

	ctx, err := metrics.RevisionContext(metric.ObjectMeta.Namespace, svcName, cfgName, revName)
	if err != nil {
		return nil, err
	}

	return &serviceScraper{
		sClient:         sClient,
		counter:         counter,
		url:             urlFromTarget(metric.Spec.ScrapeTarget, metric.ObjectMeta.Namespace),
		podAccessor:     podAccessor,
		podsAddressable: true,
		statsCtx:        ctx,
		logger:          logger,
		tickProvider:    tickProvider,
	}, nil
}

var portAndPath = strconv.Itoa(networking.AutoscalingQueueMetricsPort) + "/metrics"

func urlFromTarget(t, ns string) string {
	return fmt.Sprintf("http://%s.%s:", t, ns) + portAndPath
}

// Scrape calls the destination service then sends it
// to the given stats channel.
func (s *serviceScraper) Scrape(window time.Duration) (Stat, error) {
	readyPodsCount, err := s.counter.ReadyCount()
	if err != nil {
		return emptyStat, ErrFailedGetEndpoints
	}

	if readyPodsCount == 0 {
		return emptyStat, nil
	}

	//startTime := time.Now()
	//defer func() {
	//	scrapeTime := time.Since(startTime)
	//	pkgmetrics.RecordBatch(s.statsCtx, scrapeTimeM.M(float64(scrapeTime.Milliseconds())))
	//}()

	if s.podsAddressable {
		stat, err := s.scrapePods(readyPodsCount)
		// Some pods were scraped, but not enough.
		if err != errNoPodsScraped {
			return stat, err
		}
		// Else fall back to service scrape.
	}
	stat, err := s.scrapeService(window, readyPodsCount)
	if err == nil {
		s.logger.Info("Direct pod scraping off, service scraping, on")
		// If err == nil, this means that we failed to scrape all pods, but service worked
		// thus it is probably a mesh case.
		s.podsAddressable = false
	}
	return stat, err
}

func (s *serviceScraper) scrapePods(readyPods int) (Stat, error) {
	pods, err := s.podAccessor.PodIPsByAge()
	if err != nil {
		s.logger.Info("Error querying pods by age: ", err)
		return emptyStat, err
	}
	// Race condition when scaling to 0, where the check above
	// for endpoint count worked, but here we had no ready pods.
	if len(pods) == 0 {
		s.logger.Infof("For %s ready pods found 0 pods, are we scaling to 0?", readyPods)
		return emptyStat, nil
	}

	frpc := float64(readyPods)
	sampleSizeF := populationMeanSampleSize(frpc)
	sampleSize := int(sampleSizeF)
	results := make(chan Stat, sampleSize)

	grp := errgroup.Group{}
	idx := int32(-1)
	// Start |sampleSize| threads to scan in parallel.
	for i := 0; i < sampleSize; i++ {
		grp.Go(func() error {
			// If a given pod failed to scrape, we want to continue
			// scanning pods down the line.
			for {
				// Acquire next pod.
				myIdx := int(atomic.AddInt32(&idx, 1))
				// All out?
				if myIdx >= len(pods) {
					return errPodsExhausted
				}

				// Scrape!
				target := "http://" + pods[myIdx] + ":" + portAndPath
				stat, err := s.sClient.Scrape(target)
				if err == nil {
					results <- stat
					return nil
				}
				s.logger.Infof("Pod %s failed scraping: %v", pods[myIdx], err)
			}
		})
	}

	err = grp.Wait()
	close(results)

	// We only get here if one of the scrapers failed to scrape
	// at least one pod.
	if err != nil {
		// Got some successful pods.
		// TODO(vagababov): perhaps separate |pods| == 1 case here as well?
		if len(results) > 0 {
			s.logger.Warn("Too many pods failed scraping for meaningful interpolation")
			return emptyStat, errPodsExhausted
		}
		s.logger.Warn("0 pods were successfully scraped out of ", strconv.Itoa(len(pods)))
		// Didn't scrape a single pod, switch to service scraping.
		return emptyStat, errNoPodsScraped
	}

	return computeAverages(results, sampleSizeF, frpc), nil
}

func computeAverages(results <-chan Stat, sample, total float64) Stat {
	ret := Stat{
		Time:    time.Now(),
		PodName: scraperPodName,
	}

	// Sum the stats from individual pods.
	for stat := range results {
		ret.add(stat)
	}

	ret.average(sample, total)
	return ret
}

// scrapeService scrapes the metrics using service endpoint
// as its target, rather than individual pods.
func (s *serviceScraper) scrapeService(window time.Duration, readyPods int) (Stat, error) {
	frpc := float64(readyPods)

	sampleSizeF := populationMeanSampleSize(frpc)
	sampleSize := int(sampleSizeF)
	oldStatCh := make(chan Stat, sampleSize)
	youngStatCh := make(chan Stat, sampleSize)
	scrapedPods := &sync.Map{}

	grp := errgroup.Group{}
	youngPodCutOffSecs := window.Seconds()
	for i := 0; i < sampleSize; i++ {
		grp.Go(func() error {
			for tries := 1; ; tries++ {
				stat, err := s.tryScrape(scrapedPods)
				if err == nil {
					if stat.ProcessUptime >= youngPodCutOffSecs {
						// We run |sampleSize| goroutines and each of them terminates
						// as soon as it sees stat from an `oldPod`.
						// The channel is allocated to |sampleSize|, thus this will never
						// deadlock.
						oldStatCh <- stat
						return nil
					} else {
						select {
						// This in theory might loop over all the possible pods, thus might
						// fill up the channel.
						case youngStatCh <- stat:
						default:
							// If so, just return.
							return nil
						}
					}
				} else if tries >= scraperMaxRetries {
					// Return the error if we exhausted our retries and
					// we had an error returned (we can end up here if
					// all the pods were young, which is not an error condition).
					return err
				}
			}
		})
	}
	// Now at this point we have two possibilities.
	// 1. We scraped |sampleSize| distinct pods, with the invariant of
	// 		   sampleSize <= len(oldStatCh) + len(youngStatCh) <= sampleSize*2.
	//    Note, that `err` might still be non-nil, especially when the overall
	//    pod population is small.
	//    Consider the following case: sampleSize=3, in theory the first go routine
	//    might scrape 2 pods, the second 1 and the third won't be be able to scrape
	//		any unseen pod, so it will return `ErrDidNotReceiveStat`.
	// 2. We did not: in this case `err` below will be non-nil.

	// Return the inner error, if any.
	if err := grp.Wait(); err != nil {
		// Ignore the error if we have received enough statistics.
		if err != ErrDidNotReceiveStat || len(oldStatCh)+len(youngStatCh) < sampleSize {
			return emptyStat, fmt.Errorf("unsuccessful scrape, sampleSize=%d: %w", sampleSize, err)
		}
	}
	close(oldStatCh)
	close(youngStatCh)

	ret := Stat{
		Time:    time.Now(),
		PodName: scraperPodName,
	}

	// Sum the stats from individual pods.
	oldCnt := len(oldStatCh)
	for stat := range oldStatCh {
		ret.add(stat)
	}
	for i := oldCnt; i < sampleSize; i++ {
		// This will always succeed, see reasoning above.
		ret.add(<-youngStatCh)
	}

	ret.average(sampleSizeF, frpc)
	return ret, nil
}

// tryScrape runs a single scrape and returns stat if this is a pod that has not been
// seen before. An error otherwise or if scraping failed.
func (s *serviceScraper) tryScrape(scrapedPods *sync.Map) (Stat, error) {
	stat, err := s.sClient.Scrape(s.url)
	if err != nil {
		return emptyStat, err
	}

	if _, exists := scrapedPods.LoadOrStore(stat.PodName, struct{}{}); exists {
		return emptyStat, ErrDidNotReceiveStat
	}

	return stat, nil
}

// Report processes the collected stats and return an aggregated Stat for a revision.
func (s *bulkScraper) Report(metric *av1alpha1.Metric) (Stat, error) {
	revisionName := metric.ObjectMeta.Labels[serving.RevisionLabelKey]
	readyPodCount, err := resources.NewScopedEndpointsCounter(
		s.endpointsLister, metric.Namespace, metric.Spec.ScrapeTarget).ReadyCount()
	if err != nil {
		return emptyStat, err
	}
	ctx, err := metrics.RevisionContext(metric.ObjectMeta.Namespace, metric.Labels[serving.ServiceLabelKey], metric.Labels[serving.ConfigurationLabelKey], metric.Labels[serving.RevisionLabelKey])
	if err != nil {
		return emptyStat, err
	}
	//fmt.Printf("\n\n\n\n\n REPORT revName is %s, svcName is %s, configname is %s, ns is %s\n\n\n", metric.Labels[serving.RevisionLabelKey], metric.Labels[serving.ServiceLabelKey], metric.Labels[serving.ConfigurationLabelKey], metric.ObjectMeta.Namespace)
	startTime := time.Now()
	defer func() {
		reportBulkScrapeTime := time.Since(startTime)
		pkgmetrics.RecordBatch(ctx, reportBulkScrapeTimeM.M(float64(reportBulkScrapeTime.Milliseconds())))
	}()
	frpc := float64(readyPodCount)
	if v, ok := s.revMap.Load(revisionName); ok {
		var (
			avgConcurrency        float64
			avgProxiedConcurrency float64
			reqCount              float64
			proxiedReqCount       float64
		)
		stats := v.([]Stat)
		for _, stat := range stats {
			println(">> processing stat for: ", stat.RevisionName, stat.PodName, stat.RequestCount, stat.ProxiedRequestCount, stat.Time.String())
			avgConcurrency += stat.AverageConcurrentRequests
			avgProxiedConcurrency += stat.AverageProxiedConcurrentRequests
			reqCount += stat.RequestCount
			proxiedReqCount += stat.ProxiedRequestCount
		}
		fcount := float64(len(stats))
		avgConcurrency = avgConcurrency / fcount
		avgProxiedConcurrency = avgProxiedConcurrency / fcount
		reqCount = reqCount / fcount
		proxiedReqCount = proxiedReqCount / fcount
		stat := Stat{
			Time:                             time.Now(),
			RevisionName:                     revisionName,
			PodName:                          scraperPodName,
			AverageConcurrentRequests:        avgConcurrency * frpc,
			AverageProxiedConcurrentRequests: avgProxiedConcurrency * frpc,
			RequestCount:                     reqCount * frpc,
			ProxiedRequestCount:              proxiedReqCount * frpc,
		}
		// TODO nimak - there is probably some race here when deleting
		// revision stats
		defer s.revMap.Delete(revisionName)
		return stat, nil
	}
	return emptyStat, fmt.Errorf("no metrics for revision %s", revisionName)
}

// BulkScrape collects stats from DaemonSet scraper pod and stores the stats.
func (s *bulkScraper) BulkScrape(ctx context.Context) {
	logger := logging.FromContext(ctx)
	//revName := metric.Labels[serving.RevisionLabelKey]
	//if revName == "" {
	//	s.logger.Errorf("no Revision label found for Metric %q", metric.Name)
	//}
	//svcName := metric.Labels[serving.ServiceLabelKey]
	//cfgName := metric.Labels[serving.ConfigurationLabelKey]
	//
	//ctx, err := metrics.RevisionContext(metric.ObjectMeta.Namespace, svcName, cfgName, revName)
	//if err != nil {
	//	s.logger.Errorf("error getting RevisionContext: %v", err)
	//}
	ticker := time.NewTicker(scrapeTickInterval)
	for {
		select {
		case <-ticker.C:
			logger.Infof(">> bulk scrape ...")
			pods, err := s.podLister.Pods("knative-serving").List(labels.SelectorFromSet(labels.Set{
				"knative.dev/scraper": "devel",
			}))
			if err != nil {
				logger.Errorf("failed listing pods %v", err)
				continue
			}
			//allPods, err := s.podLister.List(labels.Everything())
			//if err != nil {
			//	logger.Errorf("failed listing allpods %v", err)
			//	continue
			//}
			//logger.Infof(">>  len(allPods): %d", len(allPods))

			logger.Infof(">> len(pods): %d", len(pods))
			grp := errgroup.Group{}
			// Pods are DS scrapers
			for _, p := range pods {
				p := p
				//metricsCtx, _ := metrics.RevisionContext(p.ObjectMeta.Namespace, metric.Labels[serving.ServiceLabelKey], metric.Labels[serving.ConfigurationLabelKey], metric.Labels[serving.RevisionLabelKey])
				grp.Go(func() error {
					startTime := time.Now()
					var revName string
					//defer func() {
					//	bulkScrapeTime := time.Since(startTime)
					//	pkgmetrics.RecordBatch(metricsCtx, bulkScrapeTimeM.M(float64(bulkScrapeTime.Milliseconds())))
					//}()
					// TODO nimak: fix the port here
					url := fmt.Sprintf("http://%s:%s/", p.Status.PodIP, "8101")
					fmt.Printf(">> scrape-url: %s\n", url)
					revMap, err := s.tryBulkScrape(url)
					if err != nil {
						fmt.Printf("error scraping %s: %v\n", url, err)
						return err
					}
					for rev, stats := range revMap {
						revName = rev
						var revStats []Stat
						if v, ok := s.revMap.Load(rev); ok {
							revStats = v.([]Stat)
						}
						revStats = append(revStats, stats...)
						s.revMap.Store(rev, revStats)
					}
					// TODOTARA HARDCODED
					if revName != "" {
						svcName := p.ObjectMeta.Labels[serving.ServiceLabelKey]
						configName := p.ObjectMeta.Labels[serving.ConfigurationLabelKey]
						//fmt.Printf("\n\n\n\n\n revName is %s, svcName is %s, configName is %s, ns is %s\n\n\n", revName, svcName, configName, p.ObjectMeta.Namespace)
						metricsCtx, _ := metrics.RevisionContext("default" /* hardcoded */, svcName, configName, revName)
						bulkScrapeTime := time.Since(startTime)
						pkgmetrics.RecordBatch(metricsCtx, bulkScrapeTimeM.M(float64(bulkScrapeTime.Milliseconds())))
					}
					return nil
				})
			}

			if err := grp.Wait(); err != nil {
				logger.Errorf("scraping failed: %w", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

func (s *bulkScraper) tryBulkScrape(url string) (map[string][]Stat, error) {
	return s.sClient.BulkScrape(url)
}

func (s *bulkScraper) Scrape(window time.Duration) (Stat, error) {
	return emptyStat, nil
}
