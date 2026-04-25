package tun

func ConfigureRoutes(name string, allowedIPs []string) error {
	return configureRoutes(name, allowedIPs)
}
