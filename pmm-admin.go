/*
	Copyright (c) 2016, Percona LLC and/or its affiliates. All rights reserved.

	This program is free software: you can redistribute it and/or modify
	it under the terms of the GNU Affero General Public License as published by
	the Free Software Foundation, either version 3 of the License, or
	(at your option) any later version.

	This program is distributed in the hope that it will be useful,
	but WITHOUT ANY WARRANTY; without even the implied warranty of
	MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
	GNU Affero General Public License for more details.

	You should have received a copy of the GNU Affero General Public License
	along with this program.  If not, see <http://www.gnu.org/licenses/>
*/

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/percona/pmm-client/pmm"
	"github.com/spf13/cobra"
)

var (
	admin pmm.Admin

	rootCmd = &cobra.Command{
		Use: "pmm-admin",
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			// This function pre-runs with every command or sub-command.
			// Two exceptions are "pmm-admin config" and "pmm-admin uninstall" which bypass it.

			// Skip pre-run for "help" command.
			// You should always be able to get help even if pmm is not configured yet.
			if cmd.Name() == "help" {
				return
			}

			// The version flag will not run anywhere else than on rootCmd as this flag is not persistent
			// and we want it only here without any additional checks.
			if flagVersion {
				fmt.Println(pmm.Version)
				os.Exit(0)
			}

			if flagFormat != "" {
				admin.Format = flagFormat
			}

			if flagJSON {
				admin.Format = "{{ json . }}"
			}

			if path := pmm.CheckBinaries(); path != "" {
				fmt.Println("Installation problem, one of the binaries is missing:", path)
				os.Exit(1)
			}

			// Read config file.
			if !pmm.FileExists(pmm.ConfigFile) {
				fmt.Println("PMM client is not configured, missing config file. Please make sure you have run 'pmm-admin config'.")
				os.Exit(1)
			}

			if err := admin.LoadConfig(); err != nil {
				fmt.Printf("Error reading config file %s: %s\n", pmm.ConfigFile, err)
				os.Exit(1)
			}

			// Check for required settings in config file
			// optional settings are marked with "omitempty"
			if admin.Config.ServerAddress == "" || admin.Config.ClientName == "" || admin.Config.ClientAddress == "" || admin.Config.BindAddress == "" {
				fmt.Println("PMM client is not configured properly. Please make sure you have run 'pmm-admin config'.")
				os.Exit(1)
			}

			switch cmd.Name() {
			case
				"info",
				"show-passwords":
				// above cmds should work w/o connectivity, so we return before admin.SetAPI()
				return
			case
				"start",
				"stop",
				"restart":
				if flagAll {
					// above cmds should work w/o connectivity if flagAll is set
					return
				}
			}

			// Set APIs and check if server is alive.
			if err := admin.SetAPI(); err != nil {
				fmt.Printf("%s\n", err)
				os.Exit(1)
			}

			// Proceed to "pmm-admin repair" if requested.
			if cmd.Name() == "repair" {
				return
			}

			// Check for broken installation.
			orphanedServices, missingServices := admin.CheckInstallation()
			if len(orphanedServices) > 0 {
				fmt.Printf(`We have found system services disconnected from PMM server.
Usually, this happens when data container is wiped before all monitoring services are removed or client is uninstalled.

Orphaned local services: %s

To continue, run 'pmm-admin repair' to remove orphaned services.
`, strings.Join(orphanedServices, ", "))
				os.Exit(1)
			}
			if len(missingServices) > 0 {
				fmt.Printf(`PMM server reports services that are missing locally.
Usually, this happens when the system is completely reinstalled.

Orphaned remote services: %s

Beware, if another system with the same client name created those services, repairing the installation will remove remote services
and the other system will be left with orphaned local services. If you are sure there is no other system with the same name,
run 'pmm-admin repair' to remove orphaned services. Otherwise, please reinstall this client.
`, strings.Join(missingServices, ", "))
				os.Exit(1)
			}
		},
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Usage()
			os.Exit(1)
		},
	}

	cmdAdd = &cobra.Command{
		Use:   "add",
		Short: "Add service to monitoring.",
		Long:  "This command is used to add a monitoring service.",
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			cmd.Root().PersistentPreRun(cmd.Root(), args)
			admin.ServiceName = admin.Config.ClientName
			admin.ServicePort = flagServicePort

			// Check if we have double dash "--"
			i := cmd.ArgsLenAtDash()
			if i == -1 {
				// If "--" is not present then first argument is Service Name and rest we pass through
				if len(args) >= 1 {
					admin.ServiceName, admin.Args = args[0], args[1:]
				}
			} else {
				// If "--" is present then we split arguments into command and exporter arguments
				// exporter arguments
				admin.Args = args[i:]

				// cmd arguments
				args = args[:i]
				if len(args) > 1 {
					fmt.Printf("Too many parameters. Only service name is allowed but got: %s.\n", strings.Join(args, ", "))
					os.Exit(1)
				}
				if len(args) == 1 {
					admin.ServiceName = args[0]
				}
			}

			if match, _ := regexp.MatchString(pmm.NameRegex, admin.ServiceName); !match {
				fmt.Println("Service name must be 2 to 60 characters long, contain only letters, numbers and symbols _ - . :")
				os.Exit(1)
			}
		},
	}

	cmdAnnotate = &cobra.Command{
		Use:     "annotate TEXT",
		Short:   "Annotate application events.",
		Long:    "Publish Application Events as Annotations to PMM Server.",
		Example: `  pmm-admin annotate "Application deploy v1.2" --tags "UI, v1.2"`,
		Run: func(cmd *cobra.Command, args []string) {
			if len(args) < 1 {
				fmt.Println("Description of annotation is required")
				os.Exit(1)
			}
			if err := admin.AddAnnotation(context.TODO(), strings.Join(args, " "), flagATags); err != nil {
				fmt.Println("Your annotation could not be posted. Error message we received was:\n", err)
				os.Exit(1)
			}
			fmt.Println("Your annotation was successfully posted.")
		},
	}

	cmdAddMySQL = &cobra.Command{
		Use:   "mysql [flags] [name]",
		Short: "Add complete monitoring for MySQL instance (linux and mysql metrics, queries).",
		Long: `This command adds the given MySQL instance to system, metrics and queries monitoring.

When adding a MySQL instance, this tool tries to auto-detect the DSN and credentials.
If you want to create a new user to be used for metrics collecting, provide --create-user option. pmm-admin will create
a new user 'pmm@' automatically using the given (auto-detected) MySQL credentials for granting purpose.

Table statistics is automatically disabled when there are more than 10000 tables on MySQL.

[name] is an optional argument, by default it is set to the client name of this PMM client.
		`,
		Example: `  pmm-admin add mysql --password abc123
  pmm-admin add mysql --password abc123 --create-user
  pmm-admin add mysql --password abc123 --port 3307 instance3307`,
		Run: func(cmd *cobra.Command, args []string) {
			// Passing additional arguments doesn't make sense because this command enables multiple monitors.
			if len(admin.Args) > 0 {
				fmt.Printf("We can't determine which monitor should receive additional flags: %s.\n", strings.Join(admin.Args, ", "))
				fmt.Println("To pass additional arguments to specific exporter you need to add it separately e.g.:")
				fmt.Println("pmm-admin add linux:metrics -- ", strings.Join(admin.Args, " "))
				fmt.Println("or")
				fmt.Println("pmm-admin add mysql:metrics -- ", strings.Join(admin.Args, " "))
				fmt.Println("or")
				fmt.Println("pmm-admin add mysql:queries -- ", strings.Join(admin.Args, " "))
				os.Exit(1)
			}

			// Check --query-source flag.
			if flagMySQLQueries.QuerySource != "auto" && flagMySQLQueries.QuerySource != "slowlog" && flagMySQLQueries.QuerySource != "perfschema" {
				fmt.Println("Flag --query-source can take the following values: auto, slowlog, perfschema.")
				os.Exit(1)
			}

			err := admin.AddLinuxMetrics(flagForce)
			if err == pmm.ErrOneLinux {
				fmt.Println("[linux:metrics] OK, already monitoring this system.")
			} else if err != nil {
				fmt.Println("[linux:metrics] Error adding linux metrics:", err)
				os.Exit(1)
			} else {
				fmt.Println("[linux:metrics] OK, now monitoring this system.")
			}

			mi, err := admin.DetectMySQL(flagM)
			if err != nil {
				fmt.Printf("[mysql:metrics] %s\n", err)
				os.Exit(1)
			}

			err = admin.AddMySQLMetrics(*mi, flagMySQLMetrics)
			if err == pmm.ErrDuplicate {
				fmt.Println("[mysql:metrics] OK, already monitoring MySQL metrics.")
			} else if err != nil {
				fmt.Println("[mysql:metrics] Error adding MySQL metrics:", err)
				os.Exit(1)
			} else {
				fmt.Println("[mysql:metrics] OK, now monitoring MySQL metrics using DSN", mi.SafeDSN)
			}

			mr, err := admin.AddMySQLQueries(*mi, flagMySQLQueries, flagQueries)
			if err == pmm.ErrDuplicate {
				fmt.Println("[mysql:queries] OK, already monitoring MySQL queries.")
			} else if err != nil {
				fmt.Println("[mysql:queries] Error adding MySQL queries:", err)
				os.Exit(1)
			} else {
				fmt.Println("[mysql:queries] OK, now monitoring MySQL queries from", mr.QuerySource,
					"using DSN", mi.SafeDSN)
			}
		},
	}
	cmdAddLinuxMetrics = &cobra.Command{
		Use:   "linux:metrics [flags] [name] [-- [exporter_args]]",
		Short: "Add this system to metrics monitoring.",
		Long: `This command adds this system to linux metrics monitoring.

You cannot monitor linux metrics from remote machines because the metric exporter requires an access to the local filesystem.
It is supposed there could be only one instance of linux metrics being monitored for this system.
However, you can add another one with the different name just for testing purposes using --force flag.

[name] is an optional argument, by default it is set to the client name of this PMM client.
[exporter_args] are the command line options to be passed directly to Prometheus Exporter.
		`,
		Run: func(cmd *cobra.Command, args []string) {
			if err := admin.AddLinuxMetrics(flagForce); err != nil {
				fmt.Println("Error adding linux metrics:", err)
				os.Exit(1)
			}
			fmt.Println("OK, now monitoring this system.")
		},
	}
	cmdAddMySQLMetrics = &cobra.Command{
		Use:   "mysql:metrics [flags] [name] [-- [exporter_args]]",
		Short: "Add MySQL instance to metrics monitoring.",
		Long: `This command adds the given MySQL instance to metrics monitoring.

When adding a MySQL instance, this tool tries to auto-detect the DSN and credentials.
If you want to create a new user to be used for metrics collecting, provide --create-user option. pmm-admin will create
a new user 'pmm@' automatically using the given (auto-detected) MySQL credentials for granting purpose.

Table statistics is automatically disabled when there are more than 10000 tables on MySQL.

[name] is an optional argument, by default it is set to the client name of this PMM client.
[exporter_args] are the command line options to be passed directly to Prometheus Exporter.
		`,
		Example: `  pmm-admin add mysql:metrics --password abc123
  pmm-admin add mysql:metrics --password abc123 --create-user
  pmm-admin add mysql:metrics --password abc123 --port 3307 instance3307
  pmm-admin add mysql:metrics --user rdsuser --password abc123 --host my-rds.1234567890.us-east-1.rds.amazonaws.com my-rds
  pmm-admin add mysql:metrics -- --collect.perf_schema.eventsstatements
  pmm-admin add mysql:metrics -- --collect.perf_schema.eventswaits=false`,
		Run: func(cmd *cobra.Command, args []string) {
			mi, err := admin.DetectMySQL(flagM)
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			if err := admin.AddMySQLMetrics(*mi, flagMySQLMetrics); err != nil {
				fmt.Println("Error adding MySQL metrics:", err)
				os.Exit(1)
			}
			fmt.Println("OK, now monitoring MySQL metrics using DSN", mi.SafeDSN)
		},
	}
	cmdAddMySQLQueries = &cobra.Command{
		Use:   "mysql:queries [flags] [name]",
		Short: "Add MySQL instance to Query Analytics.",
		Long: `This command adds the given MySQL instance to Query Analytics.

When adding a MySQL instance, this tool tries to auto-detect the DSN and credentials.
If you want to create a new user to be used for query collecting, provide --create-user option. pmm-admin will create
a new user 'pmm@' automatically using the given (auto-detected) MySQL credentials for granting purpose.

[name] is an optional argument, by default it is set to the client name of this PMM client.
		`,
		Example: `  pmm-admin add mysql:queries --password abc123
  pmm-admin add mysql:queries --password abc123 --create-user
  pmm-admin add mysql:metrics --password abc123 --port 3307 instance3307
  pmm-admin add mysql:queries --user rdsuser --password abc123 --host my-rds.1234567890.us-east-1.rds.amazonaws.com my-rds`,
		Run: func(cmd *cobra.Command, args []string) {
			// Check --query-source flag.
			if flagMySQLQueries.QuerySource != "auto" && flagMySQLQueries.QuerySource != "slowlog" && flagMySQLQueries.QuerySource != "perfschema" {
				fmt.Println("Flag --query-source can take the following values: auto, slowlog, perfschema.")
				os.Exit(1)
			}
			mi, err := admin.DetectMySQL(flagM)
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			mr, err := admin.AddMySQLQueries(*mi, flagMySQLQueries, flagQueries)
			if err != nil {
				fmt.Println("Error adding MySQL queries:", err)
				os.Exit(1)
			}
			fmt.Println("OK, now monitoring MySQL queries from", mr.QuerySource,
				"using DSN", mi.SafeDSN)
		},
	}
	cmdAddMongoDB = &cobra.Command{
		Use:   "mongodb [flags] [name]",
		Short: "Add complete monitoring for MongoDB instance (linux and mongodb metrics, queries).",
		Long: `This command adds the given MongoDB instance to system, metrics and queries monitoring.

When adding a MongoDB instance, you may provide --uri if the default one does not work for you.

[name] is an optional argument, by default it is set to the client name of this PMM client.
		`,
		Example: `  pmm-admin add mongodb
  pmm-admin add mongodb --cluster bare-metal`,
		Run: func(cmd *cobra.Command, args []string) {
			// Passing additional arguments doesn't make sense because this command enables multiple monitors.
			if len(admin.Args) > 0 {
				fmt.Printf("We can't determine which monitor should receive additional flags: %s.\n", strings.Join(admin.Args, ", "))
				fmt.Println("To pass additional arguments to specific exporter you need to add it separately e.g.:")
				fmt.Println("pmm-admin add linux:metrics -- ", strings.Join(admin.Args, " "))
				fmt.Println("or")
				fmt.Println("pmm-admin add mongodb:metrics -- ", strings.Join(admin.Args, " "))
				fmt.Println("or")
				fmt.Println("pmm-admin add mongodb:queries -- ", strings.Join(admin.Args, " "))
				os.Exit(1)
			}

			err := admin.AddLinuxMetrics(flagForce)
			if err == pmm.ErrOneLinux {
				fmt.Println("[linux:metrics]   OK, already monitoring this system.")
			} else if err != nil {
				fmt.Println("[linux:metrics]   Error adding linux metrics:", err)
				os.Exit(1)
			} else {
				fmt.Println("[linux:metrics]   OK, now monitoring this system.")
			}

			buildInfo, err := admin.DetectMongoDB(flagMongoURI)
			if err != nil {
				fmt.Printf("[mongodb:metrics] %s\n", err)
				os.Exit(1)
			}
			err = admin.AddMongoDBMetrics(flagMongoURI, flagCluster)
			if err == pmm.ErrDuplicate {
				fmt.Println("[mongodb:metrics] OK, already monitoring MongoDB metrics.")
			} else if err != nil {
				fmt.Println("[mongodb:metrics] Error adding MongoDB metrics:", err)
				os.Exit(1)
			} else {
				fmt.Println("[mongodb:metrics] OK, now monitoring MongoDB metrics using URI", pmm.SanitizeDSN(flagMongoURI))
			}
			err = admin.AddMongoDBQueries(buildInfo, flagMongoURI, flagQueries)
			if err == pmm.ErrDuplicate {
				fmt.Println("[mongodb:queries] OK, already monitoring MongoDB queries.")
			} else if err != nil {
				fmt.Println("[mongodb:queries] Error adding MongoDB queries:", err)
				os.Exit(1)
			} else {
				fmt.Println("[mongodb:queries] OK, now monitoring MongoDB queries using URI", pmm.SanitizeDSN(flagMongoURI))
				fmt.Println("[mongodb:queries] It is required for correct operation that profiling of monitored MongoDB databases be enabled.")
				fmt.Println("[mongodb:queries] Note that profiling is not enabled by default because it may reduce the performance of your MongoDB server.")
				fmt.Println("[mongodb:queries] For more information read PMM documentation (https://www.percona.com/doc/percona-monitoring-and-management/conf-mongodb.html).")
			}
		},
	}
	cmdAddMongoDBMetrics = &cobra.Command{
		Use:   "mongodb:metrics [flags] [name] [-- [exporter_args]]",
		Short: "Add MongoDB instance to metrics monitoring.",
		Long: `This command adds the given MongoDB instance to metrics monitoring.

When adding a MongoDB instance, you may provide --uri if the default one does not work for you.

[name] is an optional argument, by default it is set to the client name of this PMM client.
[exporter_args] are the command line options to be passed directly to Prometheus Exporter.
		`,
		Example: `  pmm-admin add mongodb:metrics
  pmm-admin add mongodb:metrics --cluster bare-metal
  pmm-admin add mongodb:metrics -- --mongodb.tls`,
		Run: func(cmd *cobra.Command, args []string) {
			if _, err := admin.DetectMongoDB(flagMongoURI); err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			if err := admin.AddMongoDBMetrics(flagMongoURI, flagCluster); err != nil {
				fmt.Println("Error adding MongoDB metrics:", err)
				os.Exit(1)
			}
			fmt.Println("OK, now monitoring MongoDB metrics using URI", pmm.SanitizeDSN(flagMongoURI))
		},
	}
	cmdAddMongoDBQueries = &cobra.Command{
		Use:   "mongodb:queries [flags] [name]",
		Short: "Add MongoDB instance to Query Analytics.",
		Long: `This command adds the given MongoDB instance to Query Analytics.

When adding a MongoDB instance, you may provide --uri if the default one does not work for you.

[name] is an optional argument, by default it is set to the client name of this PMM client.
		`,
		Example: `  pmm-admin add mongodb:queries
  pmm-admin add mongodb:queries`,
		Run: func(cmd *cobra.Command, args []string) {
			buildInfo, err := admin.DetectMongoDB(flagMongoURI)
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			if err := admin.AddMongoDBQueries(buildInfo, flagMongoURI, flagQueries); err != nil {
				fmt.Println("Error adding MongoDB queries:", err)
				os.Exit(1)
			}
			fmt.Println("OK, now monitoring MongoDB queries using URI", pmm.SanitizeDSN(flagMongoURI))
			fmt.Println("It is required for correct operation that profiling of monitored MongoDB databases be enabled.")
			fmt.Println("Note that profiling is not enabled by default because it may reduce the performance of your MongoDB server.")
			fmt.Println("For more information read PMM documentation (https://www.percona.com/doc/percona-monitoring-and-management/conf-mongodb.html).")
		},
	}
	cmdAddProxySQLMetrics = &cobra.Command{
		Use:   "proxysql:metrics [flags] [name] [-- [exporter_args]]",
		Short: "Add ProxySQL instance to metrics monitoring.",
		Long: `This command adds the given ProxySQL instance to metrics monitoring.

[name] is an optional argument, by default it is set to the client name of this PMM client.
[exporter_args] are the command line options to be passed directly to Prometheus Exporter.
		`,
		Run: func(cmd *cobra.Command, args []string) {
			if err := admin.DetectProxySQL(flagDSN); err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			if err := admin.AddProxySQLMetrics(flagDSN); err != nil {
				fmt.Println("Error adding proxysql metrics:", err)
				os.Exit(1)
			}
			fmt.Println("OK, now monitoring ProxySQL metrics using DSN", pmm.SanitizeDSN(flagDSN))
		},
	}
	cmdAddExternalService = &cobra.Command{
		Use:   "external:service job_name [instance] --service-port=port",
		Short: "Add external Prometheus exporter running on this host to new or existing scrape job for metrics monitoring.",
		Long: `Add external Prometheus exporter running on this host to new or existing scrape job for metrics monitoring.

[instance] is an optional argument, by default it is set to the client name of this PMM client.
		`,
		Args: cobra.RangeArgs(1, 2),
		Run: func(cmd *cobra.Command, args []string) {
			if flagServicePort == 0 {
				fmt.Println("--service-port flag is required.")
				os.Exit(1)
			}
			target := net.JoinHostPort(admin.Config.BindAddress, strconv.Itoa(flagServicePort))
			instance := admin.Config.ClientName
			if len(args) > 1 { // zeroth arg is admin.ServiceName
				instance = args[1]
			}
			exp := &pmm.ExternalMetrics{
				JobName:        admin.ServiceName,
				ScrapeInterval: flagExtInterval,
				ScrapeTimeout:  flagExtTimeout,
				MetricsPath:    flagExtPath,
				Scheme:         flagExtScheme,
				Targets: []pmm.ExternalTarget{{
					Target: target,
					Labels: []pmm.ExternalLabelPair{{
						Name:  "instance",
						Value: instance,
					}},
				}},
			}
			if err := admin.AddExternalService(context.TODO(), exp, flagForce); err != nil {
				fmt.Println("Error adding external service:", err)
				os.Exit(1)
			}
			fmt.Println("External service added.")
		},
	}
	cmdAddExternalMetrics = &cobra.Command{
		Use:   "external:metrics job_name [host1:port1[=instance1]] [host2:port2[=instance2]] ...",
		Short: "Add external Prometheus exporters job to metrics monitoring.",
		Long: `This command adds external Prometheus exporters job with given name to metrics monitoring.

An optional list of instances (scrape targets) can be provided.
		`,
		Args: cobra.MinimumNArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			if flagServicePort != 0 {
				fmt.Println("--service-port should not be used with this command.")
				os.Exit(1)
			}
			var targets []pmm.ExternalTarget
			for _, arg := range args[1:] { // zeroth arg is admin.ServiceName
				parts := strings.Split(arg, "=")
				if len(parts) > 2 {
					fmt.Printf("Unexpected syntax for %q.\n", arg)
					os.Exit(1)
				}
				target := parts[0]
				if _, _, err := net.SplitHostPort(target); err != nil {
					fmt.Printf("Unexpected syntax for %q: %s. \n", arg, err)
					os.Exit(1)
				}
				t := pmm.ExternalTarget{
					Target: target,
				}
				if len(parts) == 2 {
					// so both 1.2.3.4:9000=host1 and 1.2.3.4:9000="host1" work
					instance, _ := strconv.Unquote(parts[1])
					if instance == "" {
						instance = parts[1]
					}
					t.Labels = []pmm.ExternalLabelPair{{
						Name:  "instance",
						Value: instance,
					}}
				}
				targets = append(targets, t)
			}
			exp := &pmm.ExternalMetrics{
				JobName:        admin.ServiceName, // zeroth arg
				ScrapeInterval: flagExtInterval,
				ScrapeTimeout:  flagExtTimeout,
				MetricsPath:    flagExtPath,
				Scheme:         flagExtScheme,
				Targets:        targets,
			}
			if err := admin.AddExternalMetrics(context.TODO(), exp, !flagForce); err != nil {
				fmt.Println("Error adding external metrics:", err)
				os.Exit(1)
			}
			fmt.Println("External metrics added.")
		},
	}
	cmdAddExternalInstances = &cobra.Command{
		Use:   "external:instances job_name [host1:port1[=instance1]] [host2:port2[=instance2]] ...",
		Short: "Add external Prometheus exporters instances to existing metrics monitoring job.",
		Long: `This command adds external Prometheus exporters instances (scrape targets) to existing metrics monitoring job.
		`,
		Args: cobra.MinimumNArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			if flagServicePort != 0 {
				fmt.Println("--service-port should not be used with this command.")
				os.Exit(1)
			}
			var targets []pmm.ExternalTarget
			for _, arg := range args[1:] { // zeroth arg is admin.ServiceName
				parts := strings.Split(arg, "=")
				if len(parts) > 2 {
					fmt.Printf("Unexpected syntax for %q.\n", arg)
					os.Exit(1)
				}
				target := parts[0]
				if _, _, err := net.SplitHostPort(target); err != nil {
					fmt.Printf("Unexpected syntax for %q: %s. \n", arg, err)
					os.Exit(1)
				}
				t := pmm.ExternalTarget{
					Target: target,
				}
				if len(parts) == 2 {
					// so both 1.2.3.4:9000=host1 and 1.2.3.4:9000="host1" work
					instance, _ := strconv.Unquote(parts[1])
					if instance == "" {
						instance = parts[1]
					}
					t.Labels = []pmm.ExternalLabelPair{{
						Name:  "instance",
						Value: instance,
					}}
				}
				targets = append(targets, t)
			}
			if err := admin.AddExternalInstances(context.TODO(), admin.ServiceName, targets, !flagForce); err != nil {
				fmt.Println("Error adding external instances:", err)
				os.Exit(1)
			}
			fmt.Println("External instances added.")
		},
	}

	cmdRemove = &cobra.Command{
		Use:     "remove",
		Aliases: []string{"rm"},
		Short:   "Remove service from monitoring.",
		Long:    "This command is used to remove one monitoring service or all.",
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			cmd.Root().PersistentPreRun(cmd.Root(), args)
			admin.ServiceName = admin.Config.ClientName
			if len(args) > 0 {
				admin.ServiceName = args[0]
			}
		},
		Run: func(cmd *cobra.Command, args []string) {
			if flagAll {
				count, err := admin.RemoveAllMonitoring(false)
				if err != nil {
					fmt.Printf("Error removing one of the services: %s\n", err)
					os.Exit(1)
				}
				if count == 0 {
					fmt.Println("OK, no services found.")
				} else {
					fmt.Printf("OK, %d services were removed.\n", count)
				}
				os.Exit(0)
			}
			cmd.Usage()
			os.Exit(1)
		},
	}
	cmdRemoveMySQL = &cobra.Command{
		Use:   "mysql [flags] [name]",
		Short: "Remove all monitoring for MySQL instance (linux and mysql metrics, queries).",
		Long: `This command removes all monitoring for MySQL instance (linux and mysql metrics, queries).

[name] is an optional argument, by default it is set to the client name of this PMM client.
		`,
		Run: func(cmd *cobra.Command, args []string) {
			err := admin.RemoveLinuxMetrics()
			if err == pmm.ErrNoService {
				fmt.Printf("[linux:metrics] OK, no system %s under monitoring.\n", admin.ServiceName)
			} else if err != nil {
				fmt.Printf("[linux:metrics] Error removing linux metrics %s: %s\n", admin.ServiceName, err)
			} else {
				fmt.Printf("[linux:metrics] OK, removed system %s from monitoring.\n", admin.ServiceName)
			}

			err = admin.RemoveMySQLMetrics()
			if err == pmm.ErrNoService {
				fmt.Printf("[mysql:metrics] OK, no MySQL metrics %s under monitoring.\n", admin.ServiceName)
			} else if err != nil {
				fmt.Printf("[mysql:metrics] Error removing MySQL metrics %s: %s\n", admin.ServiceName, err)
			} else {
				fmt.Printf("[mysql:metrics] OK, removed MySQL metrics %s from monitoring.\n", admin.ServiceName)
			}

			err = admin.RemoveMySQLQueries()
			if err == pmm.ErrNoService {
				fmt.Printf("[mysql:queries] OK, no MySQL queries %s under monitoring.\n", admin.ServiceName)
			} else if err != nil {
				fmt.Printf("[mysql:queries] Error removing MySQL queries %s: %s\n", admin.ServiceName, err)
			} else {
				fmt.Printf("[mysql:queries] OK, removed MySQL queries %s from monitoring.\n", admin.ServiceName)
			}
		},
	}
	cmdRemoveLinuxMetrics = &cobra.Command{
		Use:   "linux:metrics [flags] [name]",
		Short: "Remove this system from metrics monitoring.",
		Long: `This command removes this system from linux metrics monitoring.

[name] is an optional argument, by default it is set to the client name of this PMM client.
		`,
		Run: func(cmd *cobra.Command, args []string) {
			if err := admin.RemoveLinuxMetrics(); err != nil {
				fmt.Printf("Error removing linux metrics %s: %s\n", admin.ServiceName, err)
				os.Exit(1)
			}
			fmt.Printf("OK, removed system %s from monitoring.\n", admin.ServiceName)
		},
	}
	cmdRemoveMySQLMetrics = &cobra.Command{
		Use:   "mysql:metrics [flags] [name]",
		Short: "Remove MySQL instance from metrics monitoring.",
		Long: `This command removes MySQL instance from metrics monitoring.

[name] is an optional argument, by default it is set to the client name of this PMM client.
		`,
		Run: func(cmd *cobra.Command, args []string) {
			if err := admin.RemoveMySQLMetrics(); err != nil {
				fmt.Printf("Error removing MySQL metrics %s: %s\n", admin.ServiceName, err)
				os.Exit(1)
			}
			fmt.Printf("OK, removed MySQL metrics %s from monitoring.\n", admin.ServiceName)
		},
	}
	cmdRemoveMySQLQueries = &cobra.Command{
		Use:   "mysql:queries [flags] [name]",
		Short: "Remove MySQL instance from Query Analytics.",
		Long: `This command removes MySQL instance from Query Analytics.

[name] is an optional argument, by default it is set to the client name of this PMM client.
		`,
		Run: func(cmd *cobra.Command, args []string) {
			if err := admin.RemoveMySQLQueries(); err != nil {
				fmt.Printf("Error removing MySQL queries %s: %s\n", admin.ServiceName, err)
				os.Exit(1)
			}
			fmt.Printf("OK, removed MySQL queries %s from monitoring.\n", admin.ServiceName)
		},
	}
	cmdRemoveMongoDB = &cobra.Command{
		Use:   "mongodb [flags] [name]",
		Short: "Remove all monitoring for MongoDB instance (linux and mongodb metrics).",
		Long: `This command removes all monitoring for MongoDB instance (linux and mongodb metrics).

[name] is an optional argument, by default it is set to the client name of this PMM client.
		`,
		Run: func(cmd *cobra.Command, args []string) {
			err := admin.RemoveLinuxMetrics()
			if err == pmm.ErrNoService {
				fmt.Printf("[linux:metrics]   OK, no system %s under monitoring.\n", admin.ServiceName)
			} else if err != nil {
				fmt.Printf("[linux:metrics]   Error removing linux metrics %s: %s\n", admin.ServiceName, err)
			} else {
				fmt.Printf("[linux:metrics]   OK, removed system %s from monitoring.\n", admin.ServiceName)
			}

			err = admin.RemoveMongoDBMetrics()
			if err == pmm.ErrNoService {
				fmt.Printf("[mongodb:metrics] OK, no MongoDB metrics %s under monitoring.\n", admin.ServiceName)
			} else if err != nil {
				fmt.Printf("[mongodb:metrics] Error removing MongoDB metrics %s: %s\n", admin.ServiceName, err)
			} else {
				fmt.Printf("[mongodb:metrics] OK, removed MongoDB metrics %s from monitoring.\n", admin.ServiceName)
			}

			err = admin.RemoveMongoDBQueries()
			if err == pmm.ErrNoService {
				fmt.Printf("[mongodb:queries] OK, no MongoDB queries %s under monitoring.\n", admin.ServiceName)
			} else if err != nil {
				fmt.Printf("[mongodb:queries] Error removing MongoDB queries %s: %s\n", admin.ServiceName, err)
			} else {
				fmt.Printf("[mongodb:queries] OK, removed MongoDB queries %s from monitoring.\n", admin.ServiceName)
			}
		},
	}
	cmdRemoveMongoDBMetrics = &cobra.Command{
		Use:   "mongodb:metrics [flags] [name]",
		Short: "Remove MongoDB instance from metrics monitoring.",
		Long: `This command removes MongoDB instance from metrics monitoring.

[name] is an optional argument, by default it is set to the client name of this PMM client.
		`,
		Run: func(cmd *cobra.Command, args []string) {
			if err := admin.RemoveMongoDBMetrics(); err != nil {
				fmt.Printf("Error removing MongoDB metrics %s: %s\n", admin.ServiceName, err)
				os.Exit(1)
			}
			fmt.Printf("OK, removed MongoDB metrics %s from monitoring.\n", admin.ServiceName)
		},
	}
	cmdRemoveMongoDBQueries = &cobra.Command{
		Use:   "mongodb:queries [flags] [name]",
		Short: "Remove MongoDB instance from Query Analytics.",
		Long: `This command removes MongoDB instance from Query Analytics.

[name] is an optional argument, by default it is set to the client name of this PMM client.
		`,
		Run: func(cmd *cobra.Command, args []string) {
			if err := admin.RemoveMongoDBQueries(); err != nil {
				fmt.Printf("Error removing MongoDB queries %s: %s\n", admin.ServiceName, err)
				os.Exit(1)
			}
			fmt.Printf("OK, removed MongoDB queries %s from monitoring.\n", admin.ServiceName)
		},
	}
	cmdRemoveProxySQLMetrics = &cobra.Command{
		Use:   "proxysql:metrics [flags] [name]",
		Short: "Remove ProxySQL instance from metrics monitoring.",
		Long: `This command removes ProxySQL instance from metrics monitoring.

[name] is an optional argument, by default it is set to the client name of this PMM client.
		`,
		Run: func(cmd *cobra.Command, args []string) {
			if err := admin.RemoveProxySQLMetrics(); err != nil {
				fmt.Printf("Error removing proxysql metrics %s: %s\n", admin.ServiceName, err)
				os.Exit(1)
			}
			fmt.Printf("OK, removed ProxySQL metrics %s from monitoring.\n", admin.ServiceName)
		},
	}

	cmdRemoveExternalService = &cobra.Command{
		Use:   "external:service job_name --service-port=port",
		Short: "Remove external Prometheus exporter running on this host from metrics monitoring.",
		Long:  `This command removes external Prometheus exporter running on this host from metrics monitoring.`,
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			if flagServicePort == 0 {
				fmt.Println("--service-port flag is required.")
				os.Exit(1)
			}
			target := net.JoinHostPort(admin.Config.BindAddress, strconv.Itoa(flagServicePort))
			if err := admin.RemoveExternalInstances(context.TODO(), admin.ServiceName, []string{target}); err != nil {
				fmt.Println("Error removing external service:", err)
				os.Exit(1)
			}
			fmt.Println("External service removed.")
		},
	}
	cmdRemoveExternalMetrics = &cobra.Command{
		Use:   "external:metrics job_name",
		Short: "Remove external Prometheus exporters from metrics monitoring.",
		Long:  `This command removes the given external Prometheus exporter from metrics monitoring.`,
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			if flagServicePort != 0 {
				fmt.Println("--service-port should not be used with this command.")
				os.Exit(1)
			}
			if err := admin.RemoveExternalMetrics(context.TODO(), admin.ServiceName); err != nil {
				fmt.Println("Error removing external metrics:", err)
				os.Exit(1)
			}
			fmt.Println("External metrics removed.")
		},
	}
	cmdRemoveExternalInstances = &cobra.Command{
		Use:   "external:instances job_name [host1:port1] [host1:port1] ...",
		Short: "Remove external Prometheus exporters instances from existing metrics monitoring job.",
		Long: `This command removes external Prometheus exporters instances (scrape targets) from existing metrics monitoring job.
		`,
		Args: cobra.MinimumNArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			if flagServicePort != 0 {
				fmt.Println("--service-port should not be used with this command.")
				os.Exit(1)
			}
			targets := args[1:] // zeroth arg is admin.ServiceName
			if err := admin.RemoveExternalInstances(context.TODO(), admin.ServiceName, targets); err != nil {
				fmt.Println("Error removing external instances:", err)
				os.Exit(1)
			}
			fmt.Println("External instances removed.")
		},
	}

	cmdList = &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List monitoring services for this system.",
		Long:    "This command displays the list of monitoring services and their details.",
		Run: func(cmd *cobra.Command, args []string) {
			if err := admin.List(); err != nil {
				fmt.Println("Error listing instances:", err)
				os.Exit(1)
			}
		},
	}

	cmdInfo = &cobra.Command{
		Use:   "info",
		Short: "Display PMM Client information (works offline).",
		Long:  "This command displays PMM client configuration details.",
		Run: func(cmd *cobra.Command, args []string) {
			admin.PrintInfo()
		},
	}

	cmdConfig = &cobra.Command{
		Use:   "config",
		Short: "Configure PMM Client.",
		Long: `This command configures pmm-admin to communicate with PMM server.

You can enable SSL (including self-signed certificates) and HTTP basic authentication with the server.
If HTTP authentication is enabled with the server, the same credendials will be used for all metric services
automatically to protect them.

Note, resetting of server address clears up SSL and HTTP auth options if no corresponding flags are provided.`,
		Example: `  pmm-admin config --server 192.168.56.100
  pmm-admin config --server 192.168.56.100:8000
  pmm-admin config --server 192.168.56.100 --server-password abc123`,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			// Cancel root's PersistentPreRun as we do not require config file to exist here.
			// If the config does not exist, we will init an empty and write on Run.
			if err := admin.LoadConfig(); err != nil {
				fmt.Printf("Cannot read config file %s: %s\n", pmm.ConfigFile, err)
				os.Exit(1)
			}
		},
		Run: func(cmd *cobra.Command, args []string) {
			if err := admin.SetConfig(flagC, flagForce); err != nil {
				fmt.Printf("%s\n", err)
				os.Exit(1)
			}
			fmt.Print("OK, PMM server is alive.\n\n")
			admin.ServerInfo()
		},
	}

	cmdCheckNet = &cobra.Command{
		Use:   "check-network",
		Short: "Check network connectivity between client and server.",
		Long: `This command runs the tests against PMM server to verify a bi-directional network connectivity.

* Client --> Server
Under this section you will find whether Consul, Query Analytics and Prometheus APIs are alive.
Also there is a connection performance test results with PMM server displayed.

* Client <-- Server
Here you will see the status of individual Prometheus endpoints and whether it can scrape metrics from this system.
Note, even this client can reach the server successfully it does not mean Prometheus is able to scrape from exporters.

In case, some of the endpoints are in problem state, please check if the corresponding service is running ('pmm-admin list').
If all endpoints are down here and 'pmm-admin list' shows all services are up,
please check the firewall settings whether this system allows incoming connections by address:port in question.`,
		Run: func(cmd *cobra.Command, args []string) {
			if err := admin.CheckNetwork(); err != nil {
				fmt.Println("Error checking network status:", err)
				os.Exit(1)
			}
		},
	}

	cmdPing = &cobra.Command{
		Use:   "ping",
		Short: "Check if PMM server is alive.",
		Long:  "This command verifies the connectivity with PMM server.",
		Run: func(cmd *cobra.Command, args []string) {
			// It's all good if PersistentPreRun didn't fail.
			fmt.Print("OK, PMM server is alive.\n\n")
			admin.ServerInfo()
		},
	}

	cmdShowPass = &cobra.Command{
		Use:   "show-passwords",
		Short: "Show PMM Client password information (works offline).",
		Long:  "This command shows passwords stored in the config file.",
		Run: func(cmd *cobra.Command, args []string) {
			admin.ShowPasswords()
		},
	}

	cmdStart = &cobra.Command{
		Use:   "start TYPE [flags] [name]",
		Short: "Start monitoring service.",
		Long: `This command starts the corresponding system service or all.

[name] is an optional argument, by default it is set to the client name of this PMM client.
		`,
		Example: `  pmm-admin start linux:metrics db01.vm
  pmm-admin start mysql:queries db01.vm
  pmm-admin start --all`,
		Run: func(cmd *cobra.Command, args []string) {
			if flagAll {
				numOfAffected, numOfAll, err := admin.StartStopAllMonitoring("start")
				if err != nil {
					fmt.Printf("Error starting one of the services: %s\n", err)
					os.Exit(1)
				}
				if numOfAll == 0 {
					fmt.Println("OK, no services found.")
					os.Exit(0)
				}
				if numOfAffected == 0 {
					fmt.Println("OK, all services already started. Run 'pmm-admin list' to see monitoring services.")
				} else {
					fmt.Printf("OK, started %d services.\n", numOfAffected)
				}
				// check if server is alive.
				if err := admin.SetAPI(); err != nil {
					fmt.Printf("%s\n", err)
				}
				os.Exit(0)
			}

			// Check args.
			if len(args) == 0 {
				fmt.Print("No service type specified.\n\n")
				cmd.Usage()
				os.Exit(1)
			}
			svcType := args[0]
			admin.ServiceName = admin.Config.ClientName
			if len(args) > 1 {
				admin.ServiceName = args[1]
			}

			affected, err := admin.StartStopMonitoring("start", svcType)
			if err != nil {
				fmt.Printf("Error starting %s service for %s: %s\n", svcType, admin.ServiceName, err)
				os.Exit(1)
			}
			if affected {
				fmt.Printf("OK, started %s service for %s.\n", svcType, admin.ServiceName)
			} else {
				fmt.Printf("OK, service %s already started for %s.\n", svcType, admin.ServiceName)
			}
		},
	}
	cmdStop = &cobra.Command{
		Use:   "stop TYPE [flags] [name]",
		Short: "Stop monitoring service.",
		Long: `This command stops the corresponding system service or all.

[name] is an optional argument, by default it is set to the client name of this PMM client.
		`,
		Example: `  pmm-admin stop linux:metrics db01.vm
  pmm-admin stop mysql:queries db01.vm
  pmm-admin stop --all`,
		Run: func(cmd *cobra.Command, args []string) {
			if flagAll {
				numOfAffected, numOfAll, err := admin.StartStopAllMonitoring("stop")
				if err != nil {
					fmt.Printf("Error stopping one of the services: %s\n", err)
					os.Exit(1)
				}
				if numOfAll == 0 {
					fmt.Println("OK, no services found.")
					os.Exit(0)
				}
				if numOfAffected == 0 {
					fmt.Println("OK, all services already stopped. Run 'pmm-admin list' to see monitoring services.")
				} else {
					fmt.Printf("OK, stopped %d services.\n", numOfAffected)
				}
				os.Exit(0)
			}

			// Check args.
			if len(args) == 0 {
				fmt.Print("No service type specified.\n\n")
				cmd.Usage()
				os.Exit(1)
			}
			svcType := args[0]
			admin.ServiceName = admin.Config.ClientName
			if len(args) > 1 {
				admin.ServiceName = args[1]
			}

			affected, err := admin.StartStopMonitoring("stop", svcType)
			if err != nil {
				fmt.Printf("Error stopping %s service for %s: %s\n", svcType, admin.ServiceName, err)
				os.Exit(1)
			}
			if affected {
				fmt.Printf("OK, stopped %s service for %s.\n", svcType, admin.ServiceName)
			} else {
				fmt.Printf("OK, service %s already stopped for %s.\n", svcType, admin.ServiceName)
			}
		},
	}
	cmdRestart = &cobra.Command{
		Use:   "restart TYPE [flags] [name]",
		Short: "Restart monitoring service.",
		Long: `This command restarts the corresponding system service or all.

[name] is an optional argument, by default it is set to the client name of this PMM client.
		`,
		Example: `  pmm-admin restart linux:metrics db01.vm
  pmm-admin restart mysql:queries db01.vm
  pmm-admin restart --all`,
		Run: func(cmd *cobra.Command, args []string) {
			if flagAll {
				numOfAffected, numOfAll, err := admin.StartStopAllMonitoring("restart")
				if err != nil {
					fmt.Printf("Error restarting one of the services: %s\n", err)
					os.Exit(1)
				}
				if numOfAll == 0 {
					fmt.Println("OK, no services found.")
					os.Exit(0)
				}

				fmt.Printf("OK, restarted %d services.\n", numOfAffected)
				// check if server is alive.
				if err := admin.SetAPI(); err != nil {
					fmt.Printf("%s\n", err)
				}
				os.Exit(0)
			}

			// Check args.
			if len(args) == 0 {
				fmt.Print("No service type specified.\n\n")
				cmd.Usage()
				os.Exit(1)
			}
			svcType := args[0]
			admin.ServiceName = admin.Config.ClientName
			if len(args) > 1 {
				admin.ServiceName = args[1]
			}

			if _, err := admin.StartStopMonitoring("restart", svcType); err != nil {
				fmt.Printf("Error restarting %s service for %s: %s\n", svcType, admin.ServiceName, err)
				os.Exit(1)
			}
			fmt.Printf("OK, restarted %s service for %s.\n", svcType, admin.ServiceName)
		},
	}

	cmdPurge = &cobra.Command{
		Use:   "purge TYPE [flags] [name]",
		Short: "Purge metrics data on PMM server.",
		Long: `This command purges metrics data associated with metrics service (type) on the PMM server.

It is not required that metric service or name exists.
[name] is an optional argument, by default it is set to the client name of this PMM client.
		`,
		Example: `  pmm-admin purge linux:metrics
  pmm-admin purge mysql:metrics db01.vm`,
		Run: func(cmd *cobra.Command, args []string) {
			// Check args.
			if len(args) == 0 {
				fmt.Print("No service type specified.\n\n")
				cmd.Usage()
				os.Exit(1)
			}
			svcType := args[0]
			admin.ServiceName = admin.Config.ClientName
			if len(args) > 1 {
				admin.ServiceName = args[1]
			}

			count, err := admin.PurgeMetrics(svcType)
			if err != nil {
				fmt.Printf("Error purging %s data for %s: %s\n", svcType, admin.ServiceName, err)
				os.Exit(1)
			}
			if count == 0 {
				fmt.Printf("OK, no data purged of %s for %s.\n", svcType, admin.ServiceName)
			} else {
				fmt.Printf("OK, purged %d time-series of %s data for %s.\n", count, svcType, admin.ServiceName)
			}
		},
	}

	cmdRepair = &cobra.Command{
		Use:   "repair",
		Short: "Repair installation.",
		Long: `This command removes orphaned system services.

It removes local services disconnected from PMM server and remote services that are missing locally.
		`,
		Run: func(cmd *cobra.Command, args []string) {
			if err := admin.RepairInstallation(); err != nil {
				fmt.Printf("Problem repairing the installation: %s\n", err)
				os.Exit(1)
			}
		},
	}

	cmdUninstall = &cobra.Command{
		Use:   "uninstall",
		Short: "Removes all monitoring services with the best effort.",
		Long: `This command removes all monitoring services with the best effort.

Usuaully, it runs automatically when pmm-client package is uninstalled to remove all local monitoring services
despite PMM server is alive or not.
		`,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			// Cancel root's PersistentPreRun as we do not require server to be alive.
		},
		Run: func(cmd *cobra.Command, args []string) {
			count := admin.Uninstall()
			if count == 0 {
				fmt.Println("OK, no services found.")
			} else {
				fmt.Printf("OK, %d services were removed.\n", count)
			}
			os.Exit(0)
		},
	}

	flagMongoURI, flagCluster, flagDSN, flagFormat string
	flagATags                                      string

	flagVersion, flagJSON, flagAll, flagForce bool

	flagServicePort int

	flagExtInterval, flagExtTimeout time.Duration
	flagExtPath, flagExtScheme      string

	flagM            pmm.MySQLFlags
	flagQueries      pmm.QueriesFlags
	flagMySQLMetrics pmm.MySQLMetricsFlags
	flagMySQLQueries pmm.MySQLQueriesFlags
	flagC            pmm.Config
)

func main() {
	// Commands.
	cobra.EnableCommandSorting = false
	rootCmd.AddCommand(
		cmdConfig,
		cmdAdd,
		cmdAnnotate,
		cmdRemove,
		cmdList,
		cmdInfo,
		cmdCheckNet,
		cmdPing,
		cmdStart,
		cmdStop,
		cmdRestart,
		cmdShowPass,
		cmdPurge,
		cmdRepair,
		cmdUninstall,
	)
	cmdAdd.AddCommand(
		cmdAddMySQL,
		cmdAddLinuxMetrics,
		cmdAddMySQLMetrics,
		cmdAddMySQLQueries,
		cmdAddMongoDB,
		cmdAddMongoDBMetrics,
		cmdAddMongoDBQueries,
		cmdAddProxySQLMetrics,
		cmdAddExternalService,
		cmdAddExternalMetrics,
		cmdAddExternalInstances,
	)
	cmdRemove.AddCommand(
		cmdRemoveMySQL,
		cmdRemoveLinuxMetrics,
		cmdRemoveMySQLMetrics,
		cmdRemoveMySQLQueries,
		cmdRemoveMongoDB,
		cmdRemoveMongoDBMetrics,
		cmdRemoveMongoDBQueries,
		cmdRemoveProxySQLMetrics,
		cmdRemoveExternalService,
		cmdRemoveExternalMetrics,
		cmdRemoveExternalInstances,
	)

	// Flags.
	rootCmd.PersistentFlags().StringVarP(&pmm.ConfigFile, "config-file", "c", pmm.ConfigFile, "PMM config file")
	rootCmd.PersistentFlags().BoolVarP(&admin.Verbose, "verbose", "", false, "verbose output")
	rootCmd.Flags().BoolVarP(&flagVersion, "version", "v", false, "show version")

	cmdConfig.Flags().StringVar(&flagC.ServerAddress, "server", "", "PMM server address, optionally following with the :port (default port 80 or 443 if using SSL)")
	cmdConfig.Flags().StringVar(&flagC.ClientAddress, "client-address", "", "client address, also remote/public address for this system (if omitted it will be automatically detected by asking server)")
	cmdConfig.Flags().StringVar(&flagC.BindAddress, "bind-address", "", "bind address, also local/private address that is mapped from client address via NAT/port forwarding (defaults to the client address)")
	cmdConfig.Flags().StringVar(&flagC.ClientName, "client-name", "", "client name (defaults to the system hostname)")
	cmdConfig.Flags().StringVar(&flagC.ServerUser, "server-user", "pmm", "define HTTP user configured on PMM Server")
	cmdConfig.Flags().StringVar(&flagC.ServerPassword, "server-password", "", "define HTTP password configured on PMM Server")
	cmdConfig.Flags().BoolVar(&flagC.ServerSSL, "server-ssl", false, "enable SSL to communicate with PMM Server")
	cmdConfig.Flags().BoolVar(&flagC.ServerInsecureSSL, "server-insecure-ssl", false, "enable insecure SSL (self-signed certificate) to communicate with PMM Server")
	cmdConfig.Flags().BoolVar(&flagForce, "force", false, "force to set client name on initial setup after uninstall with unreachable server")

	cmdAdd.PersistentFlags().IntVar(&flagServicePort, "service-port", 0, "service port")

	cmdAnnotate.Flags().StringVar(&flagATags, "tags", "", "List of tags (separated by comma)")

	cmdAddLinuxMetrics.Flags().BoolVar(&flagForce, "force", false, "force to add another linux:metrics instance with different name for testing purposes")

	addCommonMySQLFlags := func(cmd *cobra.Command) {
		cmd.Flags().StringVar(&flagM.DefaultsFile, "defaults-file", "", "path to my.cnf")
		cmd.Flags().StringVar(&flagM.Host, "host", "", "MySQL host")
		cmd.Flags().StringVar(&flagM.Port, "port", "", "MySQL port")
		cmd.Flags().StringVar(&flagM.User, "user", "", "MySQL username")
		cmd.Flags().StringVar(&flagM.Password, "password", "", "MySQL password")
		cmd.Flags().StringVar(&flagM.Socket, "socket", "", "MySQL socket")
		cmd.Flags().BoolVar(&flagM.CreateUser, "create-user", false, "create a new MySQL user")
		cmd.Flags().StringVar(&flagM.CreateUserPassword, "create-user-password", "", "optional password for a new MySQL user")
		cmd.Flags().Uint16Var(&flagM.MaxUserConn, "create-user-maxconn", 10, "max user connections for a new user")
		cmd.Flags().BoolVar(&flagM.Force, "force", false, "force to create/update MySQL user")
	}

	// mysql
	addCommonMySQLFlags(cmdAddMySQL)
	cmdAddMySQL.Flags().BoolVar(&flagMySQLMetrics.DisableTableStats, "disable-tablestats", false, "disable table statistics")
	cmdAddMySQL.Flags().Uint16Var(&flagMySQLMetrics.DisableTableStatsLimit, "disable-tablestats-limit", 1000, "number of tables after which table stats are disabled automatically")
	cmdAddMySQL.Flags().BoolVar(&flagMySQLMetrics.DisableUserStats, "disable-userstats", false, "disable user statistics")
	cmdAddMySQL.Flags().BoolVar(&flagMySQLMetrics.DisableBinlogStats, "disable-binlogstats", false, "disable binlog statistics")
	cmdAddMySQL.Flags().BoolVar(&flagMySQLMetrics.DisableProcesslist, "disable-processlist", false, "disable process state metrics")
	cmdAddMySQL.Flags().BoolVar(&flagQueries.DisableQueryExamples, "disable-queryexamples", false, "disable collection of query examples")
	cmdAddMySQL.Flags().BoolVar(&flagMySQLQueries.SlowLogRotation, "slow-log-rotation", true, "enable slow log rotation")
	cmdAddMySQL.Flags().IntVar(&flagMySQLQueries.RetainSlowLogs, "retain-slow-logs", 1, "number of slow logs to retain after rotation")
	cmdAddMySQL.Flags().StringVar(&flagMySQLQueries.QuerySource, "query-source", "auto", "source of SQL queries: auto, slowlog, perfschema")

	// mysql:metrics
	addCommonMySQLFlags(cmdAddMySQLMetrics)
	cmdAddMySQLMetrics.Flags().BoolVar(&flagMySQLMetrics.DisableTableStats, "disable-tablestats", false, "disable table statistics")
	cmdAddMySQLMetrics.Flags().Uint16Var(&flagMySQLMetrics.DisableTableStatsLimit, "disable-tablestats-limit", 1000, "number of tables after which table stats are disabled automatically")
	cmdAddMySQLMetrics.Flags().BoolVar(&flagMySQLMetrics.DisableUserStats, "disable-userstats", false, "disable user statistics")
	cmdAddMySQLMetrics.Flags().BoolVar(&flagMySQLMetrics.DisableBinlogStats, "disable-binlogstats", false, "disable binlog statistics")
	cmdAddMySQLMetrics.Flags().BoolVar(&flagMySQLMetrics.DisableProcesslist, "disable-processlist", false, "disable process state metrics")

	// mysql:queries
	addCommonMySQLFlags(cmdAddMySQLQueries)
	cmdAddMySQLQueries.Flags().BoolVar(&flagQueries.DisableQueryExamples, "disable-queryexamples", false, "disable collection of query examples")
	cmdAddMySQLQueries.Flags().BoolVar(&flagMySQLQueries.SlowLogRotation, "slow-log-rotation", true, "enable slow log rotation")
	cmdAddMySQLQueries.Flags().IntVar(&flagMySQLQueries.RetainSlowLogs, "retain-slow-logs", 1, "number of slow logs to retain after rotation")
	cmdAddMySQLQueries.Flags().StringVar(&flagMySQLQueries.QuerySource, "query-source", "auto", "source of SQL queries: auto, slowlog, perfschema")

	addCommonMongoDBFlags := func(cmd *cobra.Command) {
		cmd.Flags().StringVar(&flagMongoURI, "uri", "localhost:27017", "MongoDB URI, format: [mongodb://][user:pass@]host[:port][/database][?options]")
	}
	addCommonMongoDBFlags(cmdAddMongoDB)
	cmdAddMongoDB.Flags().StringVar(&flagCluster, "cluster", "", "cluster name")
	addCommonMongoDBFlags(cmdAddMongoDBMetrics)
	cmdAddMongoDBMetrics.Flags().StringVar(&flagCluster, "cluster", "", "cluster name")
	addCommonMongoDBFlags(cmdAddMongoDBQueries)
	cmdAddMongoDBQueries.Flags().BoolVar(&flagQueries.DisableQueryExamples, "disable-queryexamples", false, "disable collection of query examples")

	cmdAddProxySQLMetrics.Flags().StringVar(&flagDSN, "dsn", "stats:stats@tcp(localhost:6032)/", "ProxySQL connection DSN")

	cmdAddExternalService.Flags().DurationVar(&flagExtInterval, "interval", 0, "scrape interval")
	cmdAddExternalService.Flags().DurationVar(&flagExtTimeout, "timeout", 0, "scrape timeout")
	cmdAddExternalService.Flags().StringVar(&flagExtPath, "path", "", "metrics path")
	cmdAddExternalService.Flags().StringVar(&flagExtScheme, "scheme", "", "protocol scheme for scrapes")
	cmdAddExternalService.Flags().BoolVar(&flagForce, "force", false, "skip reachability check, overwrite scrape job parameters")

	cmdAddExternalMetrics.Flags().DurationVar(&flagExtInterval, "interval", 0, "scrape interval")
	cmdAddExternalMetrics.Flags().DurationVar(&flagExtTimeout, "timeout", 0, "scrape timeout")
	cmdAddExternalMetrics.Flags().StringVar(&flagExtPath, "path", "", "metrics path")
	cmdAddExternalMetrics.Flags().StringVar(&flagExtScheme, "scheme", "", "protocol scheme for scrapes")
	cmdAddExternalMetrics.Flags().BoolVar(&flagForce, "force", false, "skip reachability check")

	cmdAddExternalInstances.Flags().BoolVar(&flagForce, "force", false, "skip reachability check")

	cmdRemove.Flags().BoolVar(&flagAll, "all", false, "remove all monitoring services")

	cmdRemoveExternalService.Flags().IntVar(&flagServicePort, "service-port", 0, "service port")

	cmdList.Flags().StringVar(&flagFormat, "format", "", "print result using a Go template")
	cmdList.Flags().BoolVar(&flagJSON, "json", false, "print result as json")

	cmdStart.Flags().BoolVar(&flagAll, "all", false, "start all monitoring services")
	cmdStop.Flags().BoolVar(&flagAll, "all", false, "stop all monitoring services")
	cmdRestart.Flags().BoolVar(&flagAll, "all", false, "restart all monitoring services")

	if os.Getuid() != 0 {
		// skip root check if binary was build in tests
		if pmm.Version != "gotest" {
			fmt.Println("pmm-admin requires superuser privileges to manage system services.")
			os.Exit(1)
		}
	}

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
