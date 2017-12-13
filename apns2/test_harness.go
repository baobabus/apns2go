// Copyright 2017 Aleksey Blinov. All rights reserved.

package apns2

import (
	"time"

	"github.com/baobabus/go-apns/funit"
	"github.com/baobabus/go-apnsmock/apns2mock"
)

var (
	apnsMockComms_Typical = apns2mock.CommsCfg{
		MaxConcurrentStreams: 500,
		MaxConns:             1000,
		ConnectionDelay:      1*time.Second,
		ResponseTime:         20*time.Millisecond,
	}
	apnsMockComms_30ms = apns2mock.CommsCfg{
		MaxConcurrentStreams: 500,
		MaxConns:             1000,
		ConnectionDelay:      30*time.Millisecond,
		ResponseTime:         30*time.Millisecond,
	}
	apnsMockComms_NoDelay = apns2mock.CommsCfg{
		MaxConcurrentStreams: 500,
		MaxConns:             1000,
		ConnectionDelay:      0,
		ResponseTime:         0,
	}
	commsTest_Fast = CommsCfg{
		DialTimeout:          10 * time.Millisecond,
		MinDialBackOff:       4 * time.Second,
		MaxDialBackOff:       10 * time.Minute,
		DialBackOffJitter:    10 * funit.Percent,
		RequestTimeout:       20 * time.Millisecond,
		KeepAlive:            100 * time.Millisecond,
		MaxConcurrentStreams: 500,
	}

)

type tester interface {
	//Helper()
	Fatal(args ...interface{})
	Fatalf(format string, args ...interface{})
}

func mustNewMockServer(t tester) *apns2mock.Server {
	//t.Helper()
	res, err := apns2mock.NewServer(
		apnsMockComms_NoDelay,
		apns2mock.AllOkayHandler,
		apns2mock.AutoCert,
		apns2mock.AutoKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func mustNewMockServerWithCfg(t tester, cfg apns2mock.CommsCfg) *apns2mock.Server {
	//t.Helper()
	res, err := apns2mock.NewServer(
		cfg,
		apns2mock.AllOkayHandler,
		apns2mock.AutoCert,
		apns2mock.AutoKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func mustNewHTTPClient(t tester, s *apns2mock.Server) *HTTPClient {
	//t.Helper()
	res, err := NewHTTPClient(s.URL, CommsFast, nil, s.RootCertificate)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

