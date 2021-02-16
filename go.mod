module github.com/antoniomika/pcompose

go 1.15

require (
	github.com/antoniomika/sish v1.1.5
	github.com/creack/pty v1.1.11
	github.com/fsnotify/fsnotify v1.4.9
	github.com/sirupsen/logrus v1.7.0
	github.com/spf13/cobra v1.1.3
	github.com/spf13/viper v1.7.1
	golang.org/x/crypto v0.0.0-20201221181555-eec23a3978ad
	gopkg.in/natefinch/lumberjack.v2 v2.0.0
)

replace github.com/pires/go-proxyproto => github.com/antoniomika/go-proxyproto v0.1.4-0.20210215223815-7210fcdac442

replace github.com/vulcand/oxy => github.com/antoniomika/oxy v1.1.1-0.20210215225031-0afb828604bb
