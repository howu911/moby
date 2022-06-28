package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/cli"
	cliflags "github.com/docker/docker/cli/flags"
	"github.com/docker/docker/daemon"
	"github.com/docker/docker/dockerversion"
	"github.com/docker/docker/pkg/reexec"
	"github.com/docker/docker/pkg/term"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type daemonOptions struct {
	version      bool
	configFile   string
	daemonConfig *daemon.Config
	common       *cliflags.CommonOptions
	flags        *pflag.FlagSet
}

func newDaemonCommand() *cobra.Command {
	//获取配置信息
	opts := daemonOptions{
		daemonConfig: daemon.NewConfig(),
		common:       cliflags.NewCommonOptions(),
	}

	//docker daemon命令行对象，与docker client中的相似
	cmd := &cobra.Command{
		Use:           "dockerd [OPTIONS]",
		Short:         "A self-sufficient runtime for containers.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cli.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.flags = cmd.Flags()
			return runDaemon(opts)
		},
	}
	cli.SetupRootCommand(cmd)

	//解析命令
	flags := cmd.Flags()
	flags.BoolVarP(&opts.version, "version", "v", false, "Print version information and quit") //设置docker daemon启动的时候是否使用了version等一些命令
	flags.StringVar(&opts.configFile, flagDaemonConfigFile, defaultDaemonConfigFile, "Daemon configuration file")
	opts.common.InstallFlags(flags)
	opts.daemonConfig.InstallFlags(flags)
	installServiceFlags(flags)

	return cmd
}

func runDaemon(opts daemonOptions) error {
	if opts.version {
		showVersion()
		return nil
	}

	// 生成daemon对象
	daemonCli := NewDaemonCli()

	// Windows specific settings as these are not defaulted.
	if runtime.GOOS == "windows" {
		if opts.daemonConfig.Pidfile == "" {
			opts.daemonConfig.Pidfile = filepath.Join(opts.daemonConfig.Root, "docker.pid")
		}
		if opts.configFile == "" {
			opts.configFile = filepath.Join(opts.daemonConfig.Root, `config\daemon.json`)
		}
	}

	// On Windows, this may be launching as a service or with an option to
	// register the service.
	stop, err := initService(daemonCli)
	if err != nil {
		logrus.Fatal(err)
	}

	if stop {
		return nil
	}

	// daemon启动
	err = daemonCli.start(opts)
	notifyShutdown(err)
	return err
}

func showVersion() {
	fmt.Printf("Docker version %s, build %s\n", dockerversion.Version, dockerversion.GitCommit)
}

func main() {
	if reexec.Init() {
		return
	}

	// Set terminal emulation based on platform as required.
	_, stdout, stderr := term.StdStreams()
	logrus.SetOutput(stderr)

	// 构建一个docker服务器命令行接口对象，命令行接口包含了docker服务器所有可以执行的命令，并通过每一个命令结构体对象中的Run等成员函数来具体执行
	cmd := newDaemonCommand()
	cmd.SetOutput(stdout)
	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(stderr, "%s\n", err)
		os.Exit(1)
	}
}
