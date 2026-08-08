package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"moss/internal/protocol"
)

// stallRT 模拟「连得上但没响应」的公网出口——境内机器访问 ip-api / ipinfo 的常态。
// 每个请求都耗满 delay 再失败，用来量出串行重试的真实代价。
type stallRT struct{ delay time.Duration }

func (s stallRT) RoundTrip(*http.Request) (*http.Response, error) {
	time.Sleep(s.delay)
	return nil, errors.New("网络黑洞")
}

// stallGeoEndpoints 把 GeoIP 的两个客户端换成黑洞，用例结束后还原。
func stallGeoEndpoints(t *testing.T, delay time.Duration) {
	t.Helper()
	prev4, prev6 := v4HTTP, v6HTTP
	t.Cleanup(func() { v4HTTP, v6HTTP = prev4, prev6 })
	v4HTTP = &http.Client{Transport: stallRT{delay}}
	v6HTTP = &http.Client{Transport: stallRT{delay}}
}

// resetGeoState 清空 GeoIP 缓存与节流标记，用例结束后还原。
func resetGeoState(t *testing.T) {
	t.Helper()
	geoMu.Lock()
	prevCache, prevAt, prevTried := geoCache, geoCacheAt, geoTriedAt
	geoCache = struct{ ipv4, ipv6, country string }{}
	geoCacheAt, geoTriedAt = time.Time{}, time.Time{}
	geoMu.Unlock()
	t.Cleanup(func() {
		geoMu.Lock()
		geoCache, geoCacheAt, geoTriedAt = prevCache, prevAt, prevTried
		geoMu.Unlock()
	})
}

// seedGeoCache 灌入一份新鲜缓存。
func seedGeoCache(t *testing.T, ipv4, ipv6, country string) {
	t.Helper()
	resetGeoState(t)
	geoMu.Lock()
	geoCache.ipv4, geoCache.ipv6, geoCache.country = ipv4, ipv6, country
	geoCacheAt = time.Now()
	geoMu.Unlock()
}

// TestCollectInfoDoesNotWaitForGeoIP register 不能被 GeoIP 外呼拖住。
//
// publicNet 的 v4 分支依次试 ip-api → ip.sb → ipinfo，每个 5 秒超时，
// 全失败时顺序耗时可达 15 秒。collectInfo 卡在 register 之前，那 15 秒里
// agent 对 server 而言是「连上了却迟迟不注册」。30 分钟 TTL 缓存救不了这个场景:
// 首次安装、升级重启、崩溃重启都是从空缓存开始的，而这恰恰是最需要看到机器的时刻。
func TestCollectInfoDoesNotWaitForGeoIP(t *testing.T) {
	resetGeoState(t)
	stallGeoEndpoints(t, 2*time.Second)

	start := time.Now()
	info := collectInfo()
	elapsed := time.Since(start)

	// 4 秒的预算里已经给磁盘采集留了它自己的 3 秒上限；
	// 旧写法光 v4 分支串行重试就要 6 秒。
	if elapsed > 4*time.Second {
		t.Fatalf("collectInfo 被 GeoIP 外呼拖住 %v，register 会跟着一起迟到", elapsed)
	}
	if info.IP != "" || info.CountryCode != "" {
		t.Fatalf("缓存为空时不该凭空拿到公网信息: ip=%q cc=%q", info.IP, info.CountryCode)
	}
}

// TestPublicNetCachedOnlyNeverCallsOut 缓存读取路径必须彻底不碰网络。
func TestPublicNetCachedOnlyNeverCallsOut(t *testing.T) {
	resetGeoState(t)
	stallGeoEndpoints(t, 2*time.Second)

	start := time.Now()
	ipv4, ipv6, country := publicNetCachedOnly()
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("publicNetCachedOnly 外呼了，耗时 %v", elapsed)
	}
	if ipv4 != "" || ipv6 != "" || country != "" {
		t.Fatalf("缓存为空时应返回空值，实际 %q %q %q", ipv4, ipv6, country)
	}

	seedGeoCache(t, "1.2.3.4", "2001:db8::1", "jp")
	if ipv4, ipv6, country = publicNetCachedOnly(); ipv4 != "1.2.3.4" || ipv6 != "2001:db8::1" || country != "jp" {
		t.Fatalf("缓存有效时应原样返回，实际 %q %q %q", ipv4, ipv6, country)
	}
}

// TestPublicNetCachedOnlyIgnoresExpired 过期缓存不能继续用：机器换了 IP
// 或搬了机房之后，一份陈旧的国家码比没有更误导。
func TestPublicNetCachedOnlyIgnoresExpired(t *testing.T) {
	seedGeoCache(t, "1.2.3.4", "", "jp")
	geoMu.Lock()
	geoCacheAt = time.Now().Add(-geoTTL - time.Minute)
	geoMu.Unlock()

	if ipv4, _, _ := publicNetCachedOnly(); ipv4 != "" {
		t.Fatalf("超过 TTL 的缓存不应再被使用，实际拿到 %q", ipv4)
	}
}

// TestRefreshPublicNetThrottled 外呼必须节流。
//
// 重连退避最短 3 秒，而一轮全失败的外呼要烧掉约 15 秒和 3 个请求。
// 不节流的话，一台外网不通的机器会永远在空转外呼——既打免费接口的配额，
// 也让 agent 长期挂着一堆没有意义的协程。
func TestRefreshPublicNetThrottled(t *testing.T) {
	resetGeoState(t)
	stallGeoEndpoints(t, 300*time.Millisecond)

	refreshPublicNet() // 第一轮：真外呼，全部失败

	start := time.Now()
	refreshPublicNet() // 第二轮：应被节流，直接返回
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("上一轮外呼刚失败就又打了一轮，耗时 %v", elapsed)
	}
}

// TestRefreshPublicNetUsesFreshCache 缓存新鲜时不外呼。
func TestRefreshPublicNetUsesFreshCache(t *testing.T) {
	seedGeoCache(t, "1.2.3.4", "", "jp")
	stallGeoEndpoints(t, 2*time.Second)

	start := time.Now()
	ipv4, _, country := refreshPublicNet()
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("缓存仍新鲜却外呼了，耗时 %v", elapsed)
	}
	if ipv4 != "1.2.3.4" || country != "jp" {
		t.Fatalf("应返回缓存值，实际 %q %q", ipv4, country)
	}
}

// TestReregisterAfterGeoResolved 后台取到公网信息后要补发 register。
//
// 这是「register 不等 GeoIP」的另一半：不补发的话，第一次注册留下的空 IP
// 要等到下一次重连才会被纠正，而一台连接稳定的机器可能几天都不重连。
func TestReregisterAfterGeoResolved(t *testing.T) {
	seedGeoCache(t, "1.2.3.4", "2001:db8::1", "jp")
	fs := newFakeSender()

	// 首次 register 发出去时还没有任何公网信息
	refreshGeoAndReregister(context.Background(), fs, protocol.AgentInfo{})

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.msgs) != 1 {
		t.Fatalf("应补发 1 条 register，实际 %d 条", len(fs.msgs))
	}
	m := fs.msgs[0]
	if m.Type != "register" || m.Info == nil {
		t.Fatalf("补发的应是 register 消息，实际 %+v", m)
	}
	if m.Info.IP != "1.2.3.4" || m.Info.IPv6 != "2001:db8::1" || m.Info.CountryCode != "jp" {
		t.Fatalf("补发的 register 应带上公网信息，实际 ip=%q ipv6=%q cc=%q",
			m.Info.IP, m.Info.IPv6, m.Info.CountryCode)
	}
}

// TestNoReregisterWhenNothingNew 没有新信息就不该打扰 server。
func TestNoReregisterWhenNothingNew(t *testing.T) {
	seedGeoCache(t, "1.2.3.4", "2001:db8::1", "jp")
	fs := newFakeSender()

	refreshGeoAndReregister(context.Background(), fs, protocol.AgentInfo{
		IP: "1.2.3.4", IPv6: "2001:db8::1", CountryCode: "jp",
	})

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.msgs) != 0 {
		t.Fatalf("公网信息没有变化时不应补发，实际发了 %d 条", len(fs.msgs))
	}
}

// TestNoReregisterWhenGeoUnavailable 外呼失败时不补发空值——那只是徒增一次写库。
func TestNoReregisterWhenGeoUnavailable(t *testing.T) {
	resetGeoState(t)
	stallGeoEndpoints(t, 200*time.Millisecond)
	fs := newFakeSender()

	refreshGeoAndReregister(context.Background(), fs, protocol.AgentInfo{})

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.msgs) != 0 {
		t.Fatalf("取不到公网信息时不应补发，实际发了 %d 条", len(fs.msgs))
	}
}

// TestNoReregisterAfterConnectionGone 外呼期间连接断了就不该再发。
// 那条 register 属于已经死掉的连接，新连接会带着刚填好的缓存自己重新注册。
func TestNoReregisterAfterConnectionGone(t *testing.T) {
	seedGeoCache(t, "1.2.3.4", "", "jp")
	fs := newFakeSender()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	refreshGeoAndReregister(ctx, fs, protocol.AgentInfo{})

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.msgs) != 0 {
		t.Fatalf("连接已断时不应补发，实际发了 %d 条", len(fs.msgs))
	}
}
