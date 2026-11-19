package bypass.whitelist

import android.content.Intent
import android.net.Uri
import android.os.ParcelFileDescriptor
import android.util.Base64
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import androidbind.Androidbind
import androidbind.SocketProtector
import bypass.whitelist.util.Prefs
import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import java.io.File
import java.util.concurrent.atomic.AtomicInteger

/** Requires a freshly built real Go AAR. Compile-only API stubs cannot run these tests. */
@RunWith(AndroidJUnit4::class)
class RoutingImportTest {
    private fun profileLink(): String {
        val profile = JSONObject()
            .put("Name", "Routing test")
            .put("FakeDNS", "false")
            .put("GlobalProxy", "false")
            .put("DomainStrategy", "IPIfNonMatch")
            .put("RouteOrder", "block-proxy-direct")
            .put("ProxySites", JSONArray().put("geosit:ru-blocked"))
            .put("DirectIP", JSONArray().put("geoip:private"))
            .put("Geositeurl", "https://example.com/geosite.dat")
            .put("DomesticDNSType", "DoH")
            .put("DomesticDNSDomain", "https://dns.google/dns-query")
            .put("DomesticDNSIP", "8.8.8.8")
            .put("RemoteDNSType", "DoH")
            .put("RemoteDNSDomain", "https://cloudflare-dns.com/dns-query")
            .put("RemoteDNSIP", "1.1.1.1")
        return "happ://routing/add/" + Base64.encodeToString(profile.toString().toByteArray(Charsets.UTF_8), Base64.NO_WRAP)
    }

    @Test fun previewCorrectsKnownTypoButDoesNotEnableRouting() {
        val wasEnabled = Prefs.routingEnabled
        val parsed = JSONObject(Androidbind.previewRoutingProfile(profileLink()))
        assertEquals("geosite:ru-blocked", parsed.getJSONArray("ProxySites").getString(0))
        assertTrue(parsed.getJSONArray("ImportWarnings").length() > 0)
        assertEquals(wasEnabled, Prefs.routingEnabled)
    }

    @Test fun bundledLinkIsEditableAndValid() {
        val context = InstrumentationRegistry.getInstrumentation().targetContext
        val link = bypass.whitelist.util.RoutingStorage.defaultLink(context)
        val parsed = JSONObject(Androidbind.previewRoutingProfile(link))
        assertEquals("RU-SPLIT", parsed.getString("Name"))
        assertEquals(false, parsed.getBoolean("FakeDNS"))
        val edited = JSONObject(parsed.toString()).put("Name", "Edited routing")
        val roundtrip = bypass.whitelist.util.RoutingStorage.profileLink(edited.toString())
        assertEquals("Edited routing", JSONObject(Androidbind.previewRoutingProfile(roundtrip)).getString("Name"))
    }

    @Test fun newDnsDefaultsPreserveExplicitSettings() {
        val context = InstrumentationRegistry.getInstrumentation().targetContext
        val preferences = context.getSharedPreferences("app_prefs", android.content.Context.MODE_PRIVATE)
        val keys = listOf("dns_mode", "dns_primary", "dns_secondary", "routing_google_dns")
        val saved = preferences.all.filterKeys { it in keys }
        try {
            preferences.edit().apply { keys.forEach { remove(it) } }.commit()
            assertEquals(bypass.whitelist.util.DnsMode.CUSTOM, Prefs.dnsMode)
            assertEquals("8.8.8.8", Prefs.dnsPrimary)
            assertEquals("8.8.4.4", Prefs.dnsSecondary)
            assertTrue(Prefs.routingGoogleDns)
            Prefs.dnsMode = bypass.whitelist.util.DnsMode.SYSTEM
            Prefs.routingGoogleDns = false
            assertEquals(bypass.whitelist.util.DnsMode.SYSTEM, Prefs.dnsMode)
            assertEquals(false, Prefs.routingGoogleDns)
        } finally {
            preferences.edit().apply {
                keys.forEach { remove(it) }
                saved.forEach { (key, value) ->
                    when (value) { is String -> putString(key, value); is Boolean -> putBoolean(key, value) }
                }
            }.commit()
        }
    }

    @Test fun manifestAcceptsRoutingLink() {
        val context = InstrumentationRegistry.getInstrumentation().targetContext
        val intent = Intent(Intent.ACTION_VIEW, Uri.parse(profileLink()))
            .addCategory(Intent.CATEGORY_BROWSABLE)
            .setPackage(context.packageName)
        @Suppress("DEPRECATION")
        val resolved = context.packageManager.queryIntentActivities(intent, 0)
        assertTrue(resolved.any { it.activityInfo.name == MainActivity::class.java.name })
    }

    @Test fun stopBeforeStartNeverOpensOutboundSockets() {
        val context = InstrumentationRegistry.getInstrumentation().targetContext
        val pipe = ParcelFileDescriptor.createPipe()
        val fd = pipe[0].detachFd()
        val protectedCalls = AtomicInteger()
        val protector = object : SocketProtector {
            override fun protect(fd: Long): Boolean { protectedCalls.incrementAndGet(); return false }
        }
        val root = File(context.cacheDir, "not-a-routing-profile-${System.nanoTime()}").absolutePath
        val session = Androidbind.newRoutedTunSession(fd.toLong(), 1500L, 1080L, "", "", root, protector)
        try {
            session.stop()
            session.stop()
            try {
                session.start()
                throw AssertionError("Stopped session unexpectedly started")
            } catch (error: Exception) {
                assertTrue(error.message.orEmpty().contains("already stopped"))
            }
            assertEquals(0, protectedCalls.get())
        } finally {
            session.stop()
            pipe[1].close()
        }
    }
}
