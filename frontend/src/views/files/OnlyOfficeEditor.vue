<template>
  <!-- Conditionally render the DocumentEditor component -->
  <DocumentEditor v-if="ready" id="docEditor" :documentServerUrl="globalVars.onlyOfficeUrl" :config="clientConfig"
    :onLoadComponentError="onLoadComponentError"
    :events_onRequestHistory="historyEnabled ? onRequestHistory : undefined"
    :events_onRequestHistoryData="historyEnabled ? onRequestHistoryData : undefined"
    :events_onRequestHistoryClose="historyEnabled ? onRequestHistoryClose : undefined" />
  <div v-else>
    <p>{{ $t("general.loading", { suffix: "..." }) }}</p>
  </div>
  <FloatingActionButton
    icon="close"
    icon-size="1em"
    size="small"
    position="top-center"
    :slide-in="floatIn"
    :offset="{ top: '6px' }"
    interactive-zone
    :auto-hide="false"
    :label="$t('general.close', { suffix: '' })"
    @click="close"
  />
</template>

<script>
import { DocumentEditor } from "@onlyoffice/document-editor-vue";
import { globalVars } from "@/utils/constants";
import { router } from "@/router";
import { state, mutations, getters } from "@/store";
import { removeLastDir } from "@/utils/url";
import { officeApi } from "@/api";
import { toStandardLocale } from "@/i18n";
import FloatingActionButton from "@/components/settings/FloatingActionButton.vue";

export default {
  name: "onlyOfficeEditor",
  inheritAttrs: false,
  components: {
    DocumentEditor,
    FloatingActionButton,
  },
  data() {
    return {
      ready: false,
      clientConfig: {},
      floatIn: false,
      source: "",
      path: "",
      debugMode: false,
    };
  },
  computed: {
    req() {
      return state.req;
    },
    onlyOfficeUrl() {
      return globalVars.onlyOfficeUrl;
    },
    globalVars() {
      return globalVars;
    },
    // Version history is served for signed-in users only, not on shares.
    historyEnabled() {
      return Boolean(globalVars.onlyOfficeHistory) && !state.req.hash;
    },
  },
  async mounted() {
    this.source = state.req.source;
    this.path = state.req.path;

    // Show debug prompt if debug mode enabled
    if (state.user.debugOffice) {
      this.debugMode = true;
      this.showDebugPrompt();
    }

    // Perform the setup and fetch config from backend
    try {
      const configData = await officeApi.getConfig(state.req);
      configData.type = getters.isMobile() ? "mobile" : "desktop";
      this.clientConfig = configData;
      console.log("OnlyOffice client config received:", this.clientConfig);

      if (state.user?.locale !== "en") {
        this.clientConfig.editorConfig.lang = toStandardLocale(state.user.locale);
      }

      this.ready = true;

      setTimeout(() => {
        this.floatIn = true;
      }, 100);

    } catch (error) {
      console.error("Error during OnlyOffice setup:", error);
    }
  },
  beforeUnmount() {
    if (window.DocsAPI) delete window.DocsAPI;
    const iframes = document.querySelectorAll('iframe');
    for (const iframe of iframes) {
      if (iframe.src.includes('onlyoffice')) {
        iframe.remove();
      }
    }
  },
  methods: {
    close() {
      mutations.replaceRequest({});
      const uri = `${removeLastDir(state.route.path)}/`;
      const filename = this.path.split('/').pop() || "";
      void router.push({ path: uri, hash: `#${filename}` });
    },

    showDebugPrompt() {
      if (!this.debugMode) return;

      mutations.showPrompt({
        name: 'OfficeDebug',
        props: {
          onlyOfficeUrl: this.onlyOfficeUrl,
          source: this.source,
          path: this.path,
        },
      });
    },

    onLoadComponentError(errorInfo) {
      console.error("OnlyOffice component load error:", errorInfo);
    },

    docEditor() {
      return window.DocEditor?.instances?.docEditor;
    },

    async onRequestHistory() {
      try {
        const history = await officeApi.getHistory(state.req);
        this.docEditor()?.refreshHistory(history);
      } catch (error) {
        this.docEditor()?.refreshHistory({ error: error.message || String(error) });
      }
    },

    async onRequestHistoryData(event) {
      const version = event.data;
      try {
        const data = await officeApi.getHistoryData(state.req, version);
        this.docEditor()?.setHistoryData(data);
      } catch (error) {
        this.docEditor()?.setHistoryData({ error: error.message || String(error), version });
      }
    },

    // Leaving the history view: reopen the current version of the document.
    onRequestHistoryClose() {
      window.location.reload();
    },
  },
};
</script>
