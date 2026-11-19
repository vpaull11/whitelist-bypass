package bypass.whitelist.ui

import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.text.InputFilter
import android.view.LayoutInflater
import android.view.View
import android.view.ViewGroup
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView
import android.widget.Toast
import androidx.fragment.app.Fragment
import androidbind.Androidbind
import androidbind.RoutingProgress
import bypass.whitelist.R
import bypass.whitelist.tunnel.TunnelVpnService
import bypass.whitelist.util.Prefs
import bypass.whitelist.util.RoutingStorage
import com.google.android.material.button.MaterialButton
import com.google.android.material.dialog.MaterialAlertDialogBuilder
import com.google.android.material.materialswitch.MaterialSwitch
import org.json.JSONObject
import java.util.concurrent.atomic.AtomicBoolean
import kotlin.concurrent.thread

class RoutingScreenFragment : Fragment() {
    private val main = Handler(Looper.getMainLooper())
    private var cancelled = AtomicBoolean(false)
    private var working = false
    private var currentJson: String? = null
    private lateinit var summary: TextView
    private lateinit var progress: TextView
    private lateinit var input: EditText
    private lateinit var enabled: MaterialSwitch
    private lateinit var preview: MaterialButton
    private lateinit var update: MaterialButton
    private lateinit var delete: MaterialButton
    private var screen: View? = null
    private var draft: String? = null
    private var fallbackText: String? = null
    private lateinit var googleDns: MaterialSwitch
    private lateinit var restore: MaterialButton

    override fun onCreateView(inflater: LayoutInflater, container: ViewGroup?, savedInstanceState: Bundle?): View {
        val context = requireContext()
        val content = LinearLayout(context).apply {
            orientation = LinearLayout.VERTICAL
            val padding = (20 * resources.displayMetrics.density).toInt()
            setPadding(padding, padding, padding, padding)
        }
        fun text(value: String): TextView = TextView(context).apply {
            text = value
            setPadding(0, 12, 0, 12)
            content.addView(this)
        }
        fun button(label: Int): MaterialButton = MaterialButton(context).apply {
            setText(label)
            content.addView(this)
        }
        text(getString(R.string.settings_row_routing)).textSize = 22f
        enabled = MaterialSwitch(context).apply {
            setText(R.string.routing_enable)
            isChecked = Prefs.routingEnabled
            content.addView(this)
        }
        googleDns = MaterialSwitch(context).apply {
            setText(R.string.routing_google_dns)
            isChecked = Prefs.routingGoogleDns
            setOnCheckedChangeListener { _, checked -> Prefs.routingGoogleDns = checked }
            content.addView(this)
        }
        text(getString(R.string.routing_google_dns_note))
        summary = text(getString(R.string.routing_none))
        text(getString(R.string.routing_limitations))
        text(getString(R.string.routing_reconnect))
        input = EditText(context).apply {
            setHint(R.string.routing_hint)
            setSingleLine(false)
            minLines = 3
            maxLines = 6
            filters = arrayOf(InputFilter.LengthFilter(262144))
            inputType = android.text.InputType.TYPE_CLASS_TEXT or android.text.InputType.TYPE_TEXT_FLAG_MULTI_LINE or android.text.InputType.TYPE_TEXT_FLAG_NO_SUGGESTIONS
            val initial = savedInstanceState?.getString("routing_draft") ?: draft
                ?: arguments?.getString("link") ?: Prefs.routingLink.takeIf { it.isNotEmpty() }
            val default = RoutingStorage.defaultLink(context)
            fallbackText = if (initial == null) default else null
            setText(initial ?: default)
            content.addView(this)
        }
        restore = button(R.string.routing_restore).apply {
            setOnClickListener {
                MaterialAlertDialogBuilder(context)
                    .setTitle(R.string.routing_restore)
                    .setMessage(R.string.routing_restore_note)
                    .setNegativeButton(android.R.string.cancel, null)
                    .setPositiveButton(android.R.string.ok) { _, _ ->
                        input.setText(RoutingStorage.defaultLink(context))
                        fallbackText = null
                    }.show()
            }
        }
        preview = button(R.string.routing_preview)
        update = button(R.string.routing_update)
        delete = button(R.string.routing_delete)
        progress = text("")
        button(R.string.routing_cancel).setOnClickListener { cancelled.set(true) }
        return ScrollView(context).apply { addView(content); screen = this }
    }

    override fun onViewCreated(view: View, savedInstanceState: Bundle?) {
        enabled.setOnClickListener {
            val desired = enabled.isChecked
            enabled.isChecked = Prefs.routingEnabled
            if (desired && currentJson == null) return@setOnClickListener
            MaterialAlertDialogBuilder(requireContext())
                .setTitle(R.string.settings_row_routing)
                .setMessage(R.string.routing_reconnect)
                .setNegativeButton(android.R.string.cancel, null)
                .setPositiveButton(R.string.routing_apply) { _, _ ->
                    Prefs.routingEnabled = desired
                    enabled.isChecked = desired
                    setWorking(false)
                }.show()
        }
        preview.setOnClickListener {
            val link = input.text.toString().trim()
            if (!link.startsWith("happ://routing/add/")) {
                progress.text = getString(R.string.routing_hint)
                return@setOnClickListener
            }
            runCatching { Androidbind.previewRoutingProfile(link) }
                .onSuccess { confirmImport(it) }
                .onFailure { showError(it) }
        }
        update.setOnClickListener { currentJson?.let(::confirmImport) }
        delete.setOnClickListener {
            MaterialAlertDialogBuilder(requireContext()).setTitle(R.string.routing_delete)
                .setMessage(R.string.routing_reconnect).setNegativeButton(android.R.string.cancel, null)
                .setPositiveButton(R.string.routing_delete) { _, _ -> deleteProfile() }.show()
        }
        loadProfile()
    }

    private fun profileSummary(json: String): String {
        val p = JSONObject(json)
        return buildString {
            append(p.getString("Name")).append('\n')
            p.optJSONArray("ImportWarnings")?.let { warnings ->
                for (i in 0 until warnings.length()) append("Warning: ").append(warnings.getString(i)).append('\n')
            }
            for (action in listOf("Block", "Proxy", "Direct")) {
                append(action).append(": ")
                    .append((p.optJSONArray(action + "Sites")?.length() ?: 0) + (p.optJSONArray(action + "IP")?.length() ?: 0)).append('\n')
            }
            append("Order: ").append(p.getString("RouteOrder")).append('\n')
            append("Default: ").append(if (p.optBoolean("GlobalProxy")) "PROXY" else "DIRECT").append('\n')
            append("FakeDNS: off\n")
            append("Domestic DNS: ").append(p.getString("DomesticDNSDomain")).append('\n')
            append("Remote DNS: ").append(p.getString("RemoteDNSDomain")).append('\n')
            append("GeoIP: ").append(p.optString("Geoipurl")).append('\n')
            append("GeoSite: ").append(p.optString("Geositeurl")).append('\n')
            append("Profile timestamp: ").append(p.optLong("LastUpdated"))
        }
    }

    private fun confirmImport(json: String) {
        MaterialAlertDialogBuilder(requireContext()).setTitle(R.string.routing_import_confirm)
            .setMessage(profileSummary(json) + "\n\n" + getString(R.string.routing_import_note))
            .setNegativeButton(android.R.string.cancel, null)
            .setPositiveButton(R.string.routing_save) { _, _ -> download(json) }.show()
    }

    private fun setWorking(value: Boolean) {
        working = value
        preview.isEnabled = !value
        restore.isEnabled = !value
        googleDns.isEnabled = !value
        update.isEnabled = !value && currentJson != null
        delete.isEnabled = !value && currentJson != null
        // A missing/corrupt saved snapshot must not trap the user with routing
        // enabled and an unavailable toggle. Disabling stays possible.
        enabled.isEnabled = !value && (currentJson != null || Prefs.routingEnabled)
    }

    private fun loadProfile() {
        val root = RoutingStorage.directory(requireContext())
        val target = screen
        setWorking(true)
        thread(name = "routing-read") {
            val result = runCatching { Androidbind.routingProfileInfo(root) }
            main.post {
                if (screen !== target || target == null) return@post
                currentJson = result.getOrNull()
                if (currentJson != null && fallbackText != null && input.text.toString() == fallbackText) {
                    input.setText(RoutingStorage.profileLink(currentJson!!))
                }
                fallbackText = null
                summary.text = currentJson?.let(::profileSummary) ?: getString(R.string.routing_none)
                if (result.isFailure && Prefs.routingEnabled) {
                    progress.text = getString(R.string.routing_error, result.exceptionOrNull()?.message.orEmpty())
                }
                setWorking(false)
            }
        }
    }

    private fun download(json: String) {
        if (working) return
        val root = RoutingStorage.directory(requireContext())
        val target = screen
        val token = AtomicBoolean(false)
        cancelled = token
        setWorking(true)
        progress.text = "Preparing…"
        val protector = TunnelVpnService.instance?.socketProtector
        thread(name = "routing-download") {
            var lastUpdate = 0L
            val callback = object : RoutingProgress {
                override fun isCancelled(): Boolean = token.get()
                override fun onProgress(stage: String?, bytes: Long) {
                    val now = android.os.SystemClock.elapsedRealtime()
                    if (now - lastUpdate < 200 && bytes != 0L) return
                    lastUpdate = now
                    main.post {
                        if (screen === target && target != null) progress.text = "$stage: ${bytes / 1024} KiB"
                    }
                }
            }
            val result = runCatching { Androidbind.prepareRoutingProfile(root, json, callback, protector) }
            // Retain the successfully normalized editable link even if this view closed.
            result.onSuccess { Prefs.routingLink = RoutingStorage.profileLink(it) }
            main.post {
                if (screen !== target || target == null) return@post
                result.onSuccess {
                    currentJson = it
                    input.setText(Prefs.routingLink)
                    summary.text = profileSummary(it)
                    progress.text = getString(R.string.routing_saved)
                }.onFailure(::showError)
                setWorking(false)
            }
        }
    }

    private fun deleteProfile() {
        val root = RoutingStorage.directory(requireContext())
        val target = screen
        setWorking(true)
        thread(name = "routing-delete") {
            val result = runCatching { Androidbind.deleteRoutingProfile(root) }
            if (result.isSuccess) {
                Prefs.routingEnabled = false
                Prefs.routingLink = ""
            }
            main.post {
                if (screen !== target || target == null) return@post
                result.onSuccess {
                    Prefs.routingEnabled = false
                    enabled.isChecked = false
                    currentJson = null
                    input.setText(RoutingStorage.defaultLink(requireContext()))
                    summary.setText(R.string.routing_none)
                    Toast.makeText(requireContext(), R.string.routing_reconnect, Toast.LENGTH_LONG).show()
                }.onFailure(::showError)
                setWorking(false)
            }
        }
    }

    private fun showError(error: Throwable) { progress.text = getString(R.string.routing_error, error.message.orEmpty()) }
    override fun onSaveInstanceState(outState: Bundle) {
        if (::input.isInitialized) outState.putString("routing_draft", input.text.toString())
        super.onSaveInstanceState(outState)
    }

    override fun onDestroyView() {
        draft = input.text.toString()
        cancelled.set(true)
        screen = null
        super.onDestroyView()
    }
    companion object {
        fun withLink(link: String) = RoutingScreenFragment().apply { arguments = Bundle().apply { putString("link", link) } }
    }
}
