package state

// GeoCNProxySafetyDomainSuffixes prevents known polluted CN matches from
// bypassing the general proxy. Shared by legacy and Fabric compilation.
func GeoCNProxySafetyDomainSuffixes() []string {
	return []string{
		"googleapis.com",
		"gstatic.com",
		"gvt1.com",
		"gvt2.com",
		"ggpht.com",
		"googleusercontent.com",
		"googlevideo.com",
		"google.com",
		"google.com.hk",
		"youtube.com",
		"ytimg.com",
		"youtu.be",
		"app-measurement.com",
		"firebaseio.com",
		"doubleclick.net",
		"googlesyndication.com",
		"googleadservices.com",
		"google-analytics.com",
	}
}
